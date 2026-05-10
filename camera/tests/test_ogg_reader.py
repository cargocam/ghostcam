"""OGG page parser test — mirrors camera/ogg_reader_test.go behavior.

Constructs a small synthetic OGG bitstream (OpusHead + OpusTags + 3
data pages) and verifies that the reader skips the two header pages
and emits the three Opus packets.
"""

from __future__ import annotations

import asyncio
import struct

import pytest

from ghostcam.ogg_reader import read_ogg_opus_packets


def _make_ogg_page(payload_packets: list[bytes], page_seq: int) -> bytes:
    # OGG page layout:
    #   bytes 0-3: capture pattern "OggS"
    #   byte    4: stream structure version (0)
    #   byte    5: header type flags
    #   bytes 6-13: granule position (uint64 LE)
    #   bytes 14-17: bitstream serial (uint32 LE)
    #   bytes 18-21: page sequence (uint32 LE)
    #   bytes 22-25: CRC (uint32 LE) — we set 0
    #   byte   26: number of segments
    #   bytes 27..27+N: segment table
    #   bytes 27+N..: payload
    segments: list[int] = []
    payload = bytearray()
    for pkt in payload_packets:
        # A packet is split into 255-byte segments; the final segment is
        # < 255 (signaling packet end). For our short test packets one
        # segment per packet suffices.
        assert len(pkt) < 255
        segments.append(len(pkt))
        payload.extend(pkt)
    num_segments = len(segments)
    header = (
        b"OggS"
        + bytes([0])              # version
        + bytes([0])              # header flags
        + struct.pack("<Q", 0)    # granule position
        + struct.pack("<I", 0xC0FFEE)  # bitstream serial
        + struct.pack("<I", page_seq)  # page seq
        + struct.pack("<I", 0)    # CRC (skipped by reader)
        + bytes([num_segments])
        + bytes(segments)
    )
    return header + bytes(payload)


@pytest.mark.asyncio
async def test_skips_two_headers_emits_three_packets() -> None:
    opus_head = b"OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00"
    opus_tags = b"OpusTags\x06\x00\x00\x00ghost\x00\x00\x00\x00"
    pkt1 = b"opus-frame-1"
    pkt2 = b"opus-frame-2"
    pkt3 = b"opus-frame-3"

    bitstream = (
        _make_ogg_page([opus_head], 0) +
        _make_ogg_page([opus_tags], 1) +
        _make_ogg_page([pkt1], 2) +
        _make_ogg_page([pkt2], 3) +
        _make_ogg_page([pkt3], 4)
    )

    stream = asyncio.StreamReader()
    stream.feed_data(bitstream)
    stream.feed_eof()

    received: list[bytes] = []
    await read_ogg_opus_packets(stream, received.append)
    assert received == [pkt1, pkt2, pkt3]


@pytest.mark.asyncio
async def test_handles_eof_mid_stream() -> None:
    # Truncated stream — reader should return cleanly.
    stream = asyncio.StreamReader()
    stream.feed_data(b"OggS\x00\x00")  # incomplete header
    stream.feed_eof()
    await read_ogg_opus_packets(stream, lambda _: None)


@pytest.mark.asyncio
async def test_supports_async_callback() -> None:
    opus_head = b"OpusHead..."
    opus_tags = b"OpusTags..."
    pkt = b"opus-data"

    bitstream = (
        _make_ogg_page([opus_head], 0) +
        _make_ogg_page([opus_tags], 1) +
        _make_ogg_page([pkt], 2)
    )

    stream = asyncio.StreamReader()
    stream.feed_data(bitstream)
    stream.feed_eof()

    received: list[bytes] = []

    async def on_packet(p: bytes) -> None:
        await asyncio.sleep(0)  # exercise the await path
        received.append(p)

    await read_ogg_opus_packets(stream, on_packet)
    assert received == [pkt]
