"""Capture-pipeline plumbing test.

We don't run real ffmpeg here (it isn't on this CI box), but we DO
verify the load-bearing piece from Spike 2: the `pipe:{wfd}` audio
side-channel pattern works end-to-end with `asyncio.create_subprocess_exec`,
`pass_fds`, and `loop.connect_read_pipe`. A python child stands in for
ffmpeg and writes synthetic OGG pages.

This is the test that catches the regression we hit during the spikes:
the old fd-3 dup2 approach silently lost data because the child
interpreter recycled fd 3 during startup. This test asserts that the
new pattern (whatever fd the parent allocated, told to the child via
argv, kept alive by pass_fds) actually delivers bytes.
"""

from __future__ import annotations

import asyncio
import os
import struct
import sys

import pytest


def _ogg_page(packets: list[bytes], seq: int) -> bytes:
    seg_lens = []
    payload = bytearray()
    for p in packets:
        assert len(p) < 255
        seg_lens.append(len(p))
        payload.extend(p)
    return (
        b"OggS" + bytes([0, 0])
        + struct.pack("<Q", 0)
        + struct.pack("<I", 0xC0FFEE)
        + struct.pack("<I", seq)
        + struct.pack("<I", 0)
        + bytes([len(seg_lens)])
        + bytes(seg_lens)
        + bytes(payload)
    )


@pytest.mark.asyncio
async def test_audio_side_channel_via_pipe_url() -> None:
    """The `pipe:{wfd}` Spike 2 pattern: parent allocates pipe, passes
    wfd via pass_fds AND tells the child the fd number — child writes
    to it directly. Round-trip must deliver every byte."""
    rfd, wfd = os.pipe()
    os.set_inheritable(wfd, True)

    # Synthesize 3 OGG pages: opus head + tags + 1 data packet.
    pages = (
        _ogg_page([b"OpusHead..." + b"\x00" * 8], 0) +
        _ogg_page([b"OpusTags..." + b"\x00" * 8], 1) +
        _ogg_page([b"opus-frame-1"], 2)
    )

    child_code = (
        "import os, sys\n"
        f"fd = {wfd}\n"
        f"os.write(fd, {pages!r})\n"
        "os.close(fd)\n"
    )

    proc = await asyncio.create_subprocess_exec(
        sys.executable, "-c", child_code,
        pass_fds=(wfd,),
        stdout=asyncio.subprocess.DEVNULL,
        stderr=asyncio.subprocess.PIPE,
    )
    os.close(wfd)

    loop = asyncio.get_running_loop()
    stream = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(stream)
    await loop.connect_read_pipe(lambda: protocol, os.fdopen(rfd, "rb"))

    from ghostcam.ogg_reader import read_ogg_opus_packets

    received: list[bytes] = []
    await read_ogg_opus_packets(stream, received.append)

    rc = await proc.wait()
    assert rc == 0
    assert received == [b"opus-frame-1"]
