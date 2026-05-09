"""H.264 NAL Annex B parser + bounded ring buffer.

Mirrors camera/live_relay.go. Receives raw H.264 bytes via `write()`
(called from the capture pipeline tee), splits on Annex B start codes,
emits one LiveFrame per NAL unit. Audio packets are pushed separately
via `push_audio()`.

The ring is an asyncio.Queue with maxsize=ringSize. When full, the
oldest frame is dropped (matches Go's enqueue behaviour) so a slow
WebSocket consumer never back-pressures the capture pipeline.

The inner start-code scan is a single function `_find_nal_boundaries`
so a future Rust+pyo3 build can swap it via _native.find_nal_boundaries
without rewriting the asyncio plumbing. Pure Python is overwhelmingly
fast enough for the 2 Mbps Pi production rate (Spike 1 measured 883x
margin on Pi Zero 2W).
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

import asyncio

# Tunable ring depth. ~120 covers ~4 s of interleaved video+audio.
DEFAULT_RING_SIZE = 120


@dataclass(slots=True)
class LiveFrame:
    """One media frame: an H.264 NAL unit or an Opus packet."""
    data: bytes
    is_keyframe: bool = False
    is_audio: bool = False


class LiveWriter(Protocol):
    """Capture-pipeline-facing protocol satisfied by both LiveRelay and
    NullLiveRelay. Mirrors Go's LiveWriter interface."""

    def write(self, chunk: bytes) -> None: ...
    def push_audio(self, packet: bytes) -> None: ...
    def close(self) -> None: ...


class LiveRelay:
    """Annex B parser + bounded ring buffer."""

    def __init__(self, ring_size: int = DEFAULT_RING_SIZE) -> None:
        self._buf = bytearray()
        self._queue: asyncio.Queue[LiveFrame] = asyncio.Queue(maxsize=ring_size)
        self._closed = False

    @property
    def queue(self) -> asyncio.Queue[LiveFrame]:
        """Consumer reads from this queue. The WebSocket relay loops on it."""
        return self._queue

    def write(self, chunk: bytes) -> None:
        """Called from the capture-pipeline tee. Sync because the tee is
        a `loop.subprocess_exec`-driven copy task that hands off chunks
        as they come."""
        if self._closed:
            return
        self._buf.extend(chunk)
        self._flush(final=False)

    def push_audio(self, packet: bytes) -> None:
        """Enqueue an Opus audio frame. Called by the OGG reader."""
        if self._closed or not packet:
            return
        self._enqueue(LiveFrame(data=bytes(packet), is_audio=True))

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._flush(final=True)
        # Sentinel: a non-blocking put attempts None — but since None
        # isn't a LiveFrame, consumers that respect the protocol stop on
        # close() being called externally. Real shutdown happens via
        # task cancellation, so we just stop accepting new frames.

    # --- internals ---

    def _flush(self, *, final: bool) -> None:
        while True:
            start = _find_start_code(self._buf, 0)
            if start < 0:
                if final and self._buf:
                    self._emit_video(bytes(self._buf))
                    self._buf = bytearray()
                return

            if start > 0:
                del self._buf[:start]

            sc_len = _start_code_len(self._buf, 0)
            end = _find_start_code(self._buf, sc_len)
            if end < 0:
                if final:
                    self._emit_video(bytes(self._buf[sc_len:]))
                    self._buf = bytearray()
                return

            self._emit_video(bytes(self._buf[sc_len:end]))
            del self._buf[:end]

    def _emit_video(self, data: bytes) -> None:
        if not data:
            return
        self._enqueue(LiveFrame(data=data, is_keyframe=_is_idr(data[0])))

    def _enqueue(self, frame: LiveFrame) -> None:
        # Drop-oldest semantics. Matches Go's two-step nonblocking
        # enqueue: try put, if full drop one and try put again.
        try:
            self._queue.put_nowait(frame)
            return
        except asyncio.QueueFull:
            pass
        try:
            self._queue.get_nowait()
        except asyncio.QueueEmpty:
            pass
        try:
            self._queue.put_nowait(frame)
        except asyncio.QueueFull:
            pass


class NullLiveRelay:
    """No-op writer. Used when live streaming is disabled."""

    def write(self, chunk: bytes) -> None: ...
    def push_audio(self, packet: bytes) -> None: ...
    def close(self) -> None: ...


# --- raw byte-level helpers (Rust-replaceable hot path) ---

_SC3 = b"\x00\x00\x01"


def _find_start_code(buf: bytes | bytearray | memoryview, offset: int) -> int:
    """Locate the next Annex B start code at offset+. Returns the index of
    the leading byte (the leading 0x00 of either 0x00000001 or 0x000001).
    -1 if none.

    Mirrors camera/live_relay.go::findStartCode. We use `bytes.find` for
    the inner scan and then promote to the 4-byte form when the byte
    immediately before the 3-byte hit (and within the offset window) is
    also 0x00. Pure Python is fast enough at Pi production rates
    (Spike 1 measured ~883 MB/s on x86, ~221 MB/s extrapolated to
    Pi Zero 2W; the camera produces ~0.25 MB/s).
    """
    pos = bytes(buf).find(_SC3, offset) if not isinstance(buf, bytes) \
        else buf.find(_SC3, offset)
    if pos < 0:
        return -1
    if pos > offset and buf[pos - 1] == 0:
        return pos - 1
    return pos


def _start_code_len(buf: bytes | bytearray | memoryview, start: int) -> int:
    """3 for 0x000001, 4 for 0x00000001. Mirrors Go's startCodeLen which
    is called on a buffer where the start code is at the beginning."""
    if start + 3 < len(buf) \
            and buf[start] == 0 and buf[start + 1] == 0 \
            and buf[start + 2] == 0 and buf[start + 3] == 1:
        return 4
    return 3


def _is_idr(first_nal_byte: int) -> bool:
    """NAL unit type is the low 5 bits of the first byte after the start
    code. Type 5 = IDR slice (keyframe)."""
    return (first_nal_byte & 0x1F) == 5
