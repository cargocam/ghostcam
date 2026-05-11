"""Segment watcher behavior — sync byte validation, mtime quiescence,
local-storage-cap eviction, pending-confirm seeding.

Mirrors the behavioural invariants in camera/watcher.go without setting
up a full ffmpeg pipeline (we craft .ts files by hand).
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import time
from pathlib import Path

import pytest

from ghostcam.watcher import (
    NewSegment,
    enforce_local_storage_cap,
    run_segment_watcher,
)


def _write_ts(path: Path, size: int = 256, valid: bool = True) -> None:
    leading = b"\x47" if valid else b"\x00"
    body = leading + b"\x00" * (size - 1)
    path.write_bytes(body)


def _age_file(path: Path, seconds: int) -> None:
    """Backdate atime/mtime so the watcher considers the file 'finished'."""
    past = time.time() - seconds
    os.utime(path, (past, past))


def test_enforce_local_storage_cap_evicts_oldest(tmp_path: Path) -> None:
    paths = []
    for i in range(5):
        p = tmp_path / f"seg{i:05d}.ts"
        _write_ts(p, size=1024)
        _age_file(p, seconds=100 - i)  # earlier files have older mtime
        paths.append(p)

    # Cap to 2 KB — only ~2 newest files should survive.
    enforce_local_storage_cap(tmp_path, cap_bytes=2048)

    surviving = sorted(p.name for p in tmp_path.iterdir())
    # The newer files (higher i) have more recent mtime, so they remain.
    assert surviving == ["seg00003.ts", "seg00004.ts"]


def test_enforce_local_storage_cap_zero_disables(tmp_path: Path) -> None:
    p = tmp_path / "seg00001.ts"
    _write_ts(p, size=1024)
    enforce_local_storage_cap(tmp_path, cap_bytes=0)
    assert p.exists()


@pytest.mark.asyncio
async def test_watcher_picks_up_quiesced_segment(tmp_path: Path) -> None:
    seg_dir = tmp_path / "segments"
    seg_dir.mkdir()
    data_dir = tmp_path / "data"
    data_dir.mkdir()

    p = seg_dir / "seg00001.ts"
    _write_ts(p, size=512)
    _age_file(p, seconds=10)  # > 2 s mtime quiescence

    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    task = asyncio.create_task(run_segment_watcher(seg_dir, data_dir, 0, queue))
    try:
        # Watcher poll interval is 2 s; allow up to 4 s.
        seg = await asyncio.wait_for(queue.get(), timeout=4.0)
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    assert seg.filename == "seg00001.ts"
    assert seg.path == p
    assert seg.size_bytes == 512


@pytest.mark.asyncio
async def test_watcher_skips_corrupt_ts(tmp_path: Path) -> None:
    seg_dir = tmp_path / "segments"
    seg_dir.mkdir()
    data_dir = tmp_path / "data"
    data_dir.mkdir()

    bad = seg_dir / "bad.ts"
    _write_ts(bad, size=128, valid=False)
    _age_file(bad, seconds=10)

    good = seg_dir / "good.ts"
    _write_ts(good, size=128)
    _age_file(good, seconds=10)

    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    task = asyncio.create_task(run_segment_watcher(seg_dir, data_dir, 0, queue))
    try:
        seg = await asyncio.wait_for(queue.get(), timeout=4.0)
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    assert seg.filename == "good.ts"


@pytest.mark.asyncio
async def test_watcher_seeds_known_from_pending_confirms(tmp_path: Path) -> None:
    """Pre-existing segments listed in pending_confirms.json should NOT
    be re-queued — they're already at S3 awaiting server confirm."""
    seg_dir = tmp_path / "segments"
    seg_dir.mkdir()
    data_dir = tmp_path / "data"
    data_dir.mkdir()

    # Segment that's already pending confirm.
    confirmed = seg_dir / "abc-123.ts"
    _write_ts(confirmed)
    _age_file(confirmed, seconds=10)

    (data_dir / "pending_confirms.json").write_text(json.dumps([
        {
            "segment_id": "abc-123",
            "start_ts": 1,
            "end_ts": 2,
            "size_bytes": 3,
            "has_motion": False,
        },
    ]))

    # A new segment that should be picked up.
    fresh = seg_dir / "seg00002.ts"
    _write_ts(fresh)
    _age_file(fresh, seconds=10)

    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    task = asyncio.create_task(run_segment_watcher(seg_dir, data_dir, 0, queue))
    try:
        seg = await asyncio.wait_for(queue.get(), timeout=4.0)
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    # Only the fresh one should make it onto the queue.
    assert seg.filename == "seg00002.ts"
    assert queue.empty()
