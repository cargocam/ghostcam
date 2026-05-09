"""Annex B start-code detection + ring-buffer drop-oldest semantics.

Mirrors camera/live_relay_test.go. Asserts byte-identical NAL boundary
behavior — the WebSocket relay sends one NAL per binary frame, so a
miscount here turns into mid-stream decode failures that are very hard
to diagnose in production.
"""

from __future__ import annotations

import pytest

from ghostcam.live_relay import (
    DEFAULT_RING_SIZE,
    LiveRelay,
    NullLiveRelay,
    _find_start_code,
    _is_idr,
    _start_code_len,
)


def test_find_start_code_3byte() -> None:
    buf = b"\xff\xff\x00\x00\x01\xab\xcd"
    assert _find_start_code(buf, 0) == 2


def test_find_start_code_4byte() -> None:
    # 0x00 0x00 0x00 0x01 — 4-byte SC at index 0.
    buf = b"\x00\x00\x00\x01\xab\xcd"
    pos = _find_start_code(buf, 0)
    assert pos == 0
    assert _start_code_len(buf, pos) == 4


def test_find_start_code_4byte_offset_promotion() -> None:
    # Buffer has trailing 0x00 from a previous NAL, then 0x00 0x00 0x01.
    # bytes.find returns the index of the 0x00 0x00 0x01 trio, but if
    # the byte before is also 0x00 (and not before our offset), promote
    # to the 4-byte form. This matches Go's findStartCode.
    buf = b"\xff\x00\x00\x00\x01\xab"
    pos = _find_start_code(buf, 0)
    assert pos == 1
    assert _start_code_len(buf, pos) == 4


def test_find_start_code_offset_blocks_promotion() -> None:
    # When the leading 0x00 lives BEFORE the offset, Go's findStartCode
    # treats it as a 3-byte SC starting at offset.
    buf = b"\x00\x00\x00\x01\xab"
    pos = _find_start_code(buf, 1)
    assert pos == 1
    # bytes BEFORE offset don't promote → 3-byte SC.
    assert _start_code_len(buf, pos) == 3


def test_find_start_code_none() -> None:
    assert _find_start_code(b"\x00\x00\x00\x00\x02", 0) == -1
    assert _find_start_code(b"", 0) == -1


def test_is_idr() -> None:
    # NAL type 5 = IDR slice. NAL type is low 5 bits of the first byte.
    assert _is_idr(0x65)  # 0b01100101 → type 5
    assert _is_idr(0x05)
    assert not _is_idr(0x61)  # type 1 = non-IDR slice
    assert not _is_idr(0x67)  # type 7 = SPS
    assert not _is_idr(0x68)  # type 8 = PPS


@pytest.mark.asyncio
async def test_relay_emits_one_frame_per_nal() -> None:
    relay = LiveRelay(ring_size=16)
    # SPS, PPS, IDR — three NAL units separated by 0x000001.
    sps = b"\x67\x42\x00\x1f"
    pps = b"\x68\xce\x06\xe2"
    idr = b"\x65" + b"\xab" * 64
    bytestream = (
        b"\x00\x00\x00\x01" + sps +
        b"\x00\x00\x00\x01" + pps +
        b"\x00\x00\x00\x01" + idr +
        b"\x00\x00\x00\x01"  # trailing SC, expect three full NALs
    )

    relay.write(bytestream)

    out = []
    while not relay.queue.empty():
        out.append(relay.queue.get_nowait())

    assert [f.data for f in out] == [sps, pps, idr]
    assert [f.is_keyframe for f in out] == [False, False, True]
    assert all(not f.is_audio for f in out)


@pytest.mark.asyncio
async def test_relay_handles_split_writes() -> None:
    relay = LiveRelay(ring_size=8)
    nal1 = b"\x67\x42"
    nal2 = b"\x68\xce"
    full = b"\x00\x00\x00\x01" + nal1 + b"\x00\x00\x00\x01" + nal2 + b"\x00\x00\x00\x01"

    # Split arbitrarily across writes.
    relay.write(full[:3])
    relay.write(full[3:9])
    relay.write(full[9:])

    frames = []
    while not relay.queue.empty():
        frames.append(relay.queue.get_nowait())
    assert [f.data for f in frames] == [nal1, nal2]


@pytest.mark.asyncio
async def test_drop_oldest_when_ring_full() -> None:
    relay = LiveRelay(ring_size=2)
    # Push 5 NALs into a 2-slot ring; only the last 2 should survive.
    for i in range(5):
        relay.push_audio(bytes([i]))

    out = []
    while not relay.queue.empty():
        out.append(relay.queue.get_nowait())
    assert len(out) == 2
    # Last two pushes survive — drop-oldest semantics.
    assert [f.data[0] for f in out] == [3, 4]


def test_default_ring_size_matches_go() -> None:
    # camera/main.go: NewLiveRelay(120). Documented as ~4 s at 30 fps
    # of interleaved video+audio.
    assert DEFAULT_RING_SIZE == 120


def test_null_relay_is_noop() -> None:
    null = NullLiveRelay()
    null.write(b"\x00\x00\x00\x01\x67")
    null.push_audio(b"opus")
    null.close()
