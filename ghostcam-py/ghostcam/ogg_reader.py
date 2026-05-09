"""Minimal OGG page parser → Opus packets.

Mirrors camera/ogg_reader.go. Just enough to extract Opus packets from
ffmpeg's `-f ogg -c:a libopus` output. No CRC validation; no chained
streams.

Async because the page bytes arrive from an asyncio.StreamReader that
wraps the ffmpeg side-channel pipe (see capture.py for how the pipe is
set up — the `pipe:{wfd}` URL pattern from Spike 2).
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable

logger = logging.getLogger(__name__)


async def read_ogg_opus_packets(
    stream: asyncio.StreamReader,
    on_packet: Callable[[bytes], Awaitable[None] | None],
) -> None:
    """Read pages until EOF, dispatching each Opus packet.

    Skips the first two pages (OpusHead + OpusTags). on_packet may be
    sync or async — both forms are awaited correctly.
    """
    header_pages = 0
    while True:
        try:
            packets = await _read_one_page(stream)
        except asyncio.IncompleteReadError:
            return  # EOF
        except OggError as e:
            logger.debug("ogg parse error: %s", e)
            return

        if header_pages < 2:
            header_pages += 1
            continue

        for pkt in packets:
            if not pkt:
                continue
            result = on_packet(pkt)
            if asyncio.iscoroutine(result):
                await result


class OggError(ValueError):
    pass


async def _read_one_page(stream: asyncio.StreamReader) -> list[bytes]:
    hdr = await stream.readexactly(27)
    if hdr[:4] != b"OggS":
        raise OggError(f"bad capture pattern: {hdr[:4]!r}")
    num_segments = hdr[26]
    seg_table = await stream.readexactly(num_segments) if num_segments else b""
    total = sum(seg_table)
    data = await stream.readexactly(total) if total else b""

    packets: list[bytes] = []
    current = bytearray()
    offset = 0
    for seg_len in seg_table:
        current.extend(data[offset:offset + seg_len])
        offset += seg_len
        if seg_len < 255:
            packets.append(bytes(current))
            current = bytearray()
    return packets
