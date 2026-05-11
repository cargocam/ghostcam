"""Segment directory watcher.

Mirrors camera/watcher.go. Polls segment_dir every 2 s, picks up newly
finished .ts files (mtime older than 2 s, sync byte 0x47 valid, size
non-zero), runs motion detection, and pushes a NewSegment dataclass
onto the upload queue.

A fresh start seeds the "known" set from pending_confirms.json — those
segments are already at S3 awaiting confirm — so any other on-disk .ts
files are treated as orphans and re-queued.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from ghostcam.motion import MotionDetector
from ghostcam.upload import PendingConfirms

if TYPE_CHECKING:
    from ghostcam.segment_index import SegmentIndex

SEGMENT_DURATION_SECS = 6
SCAN_INTERVAL_SECS = 2.0
MTIME_QUIESCENCE_MS = 2000  # file is "still being written" if newer than this
QUEUE_PUT_TIMEOUT = 5.0

logger = logging.getLogger(__name__)


@dataclass
class NewSegment:
    filename: str
    path: Path
    start_ts: int  # Unix milliseconds
    end_ts: int    # Unix milliseconds
    size_bytes: int
    has_motion: bool = False
    retry_count: int = 0


@dataclass
class _Candidate:
    name: str
    path: Path
    mtime_ms: int
    size: int


@dataclass
class _WatcherState:
    known: set[str] = field(default_factory=set)
    detector: MotionDetector = field(default_factory=MotionDetector)


def enforce_local_storage_cap(directory: Path, cap_bytes: int) -> None:
    """Delete oldest .ts files until total size is under cap_bytes.

    Mirrors camera/watcher.go::EnforceLocalStorageCap.
    """
    if cap_bytes == 0:
        return
    try:
        entries = list(directory.iterdir())
    except OSError:
        return

    files: list[tuple[Path, int, float]] = []
    total = 0
    for e in entries:
        if e.suffix != ".ts":
            continue
        try:
            stat = e.stat()
        except OSError:
            continue
        files.append((e, stat.st_size, stat.st_mtime))
        total += stat.st_size

    if total <= cap_bytes:
        return

    files.sort(key=lambda t: t[2])  # oldest first
    for path, size, _ in files:
        if total <= cap_bytes:
            return
        try:
            path.unlink()
            total -= size
            logger.debug("evicted local segment: %s (-%d bytes)", path.name, size)
        except OSError:
            pass


def _is_valid_ts(path: Path) -> bool:
    """First byte must be the MPEG-TS sync byte 0x47."""
    try:
        with path.open("rb") as f:
            return f.read(1) == b"\x47"
    except OSError:
        return False


async def run_segment_watcher(
    segment_dir: Path,
    data_dir: Path,
    local_storage_cap_bytes: int,
    out: asyncio.Queue[NewSegment],
    *,
    index: SegmentIndex | None = None,
) -> None:
    """Poll-loop. Cancel via task cancellation.

    `index` (optional) is the SQLite-backed segment manifest. When
    provided, seeds the known-set from there (covering both pending-
    confirm and lazy-mode local-only segments) and prefers index-driven
    eviction over the simpler mtime-based cap. Falls back to the
    legacy `pending_confirms.json` path when None for tests / partial
    setups.
    """
    state = _WatcherState()

    if index is not None:
        for name in index.known_filenames():
            state.known.add(name)
        if state.known:
            logger.info(
                "segment watcher: seeded %d known from index", len(state.known),
            )
    else:
        pending = PendingConfirms(data_dir)
        confirms = pending.load()
        for c in confirms:
            state.known.add(f"{c.segment_id}.ts")
        if confirms:
            logger.info(
                "segment watcher: seeded %d known from pending confirms",
                len(confirms),
            )

    try:
        entries = list(segment_dir.iterdir())
        orphaned = sum(
            1 for e in entries
            if e.suffix == ".ts" and e.name not in state.known
        )
        if orphaned:
            logger.info("segment watcher: %d orphaned segments will be re-uploaded", orphaned)
    except OSError:
        pass

    while True:
        if index is not None:
            _enforce_cap_via_index(segment_dir, local_storage_cap_bytes, index)
        else:
            enforce_local_storage_cap(segment_dir, local_storage_cap_bytes)
        await _scan_once(segment_dir, state, out)
        await asyncio.sleep(SCAN_INTERVAL_SECS)


def _enforce_cap_via_index(
    directory: Path,
    cap_bytes: int,
    index: SegmentIndex,
) -> None:
    """Eviction that respects the SegmentIndex tier order:
    uploaded+confirmed → uploaded → local-only no-motion → motion.

    Walks `eviction_candidates()` until total on-disk size is under
    cap. Mirrors the simpler `enforce_local_storage_cap` semantics
    but with motion-friendly priority — losing motion footage is the
    last resort, never the first.
    """
    if cap_bytes == 0:
        return
    try:
        total = sum(
            e.stat().st_size
            for e in directory.iterdir()
            if e.suffix == ".ts" and e.is_file()
        )
    except OSError:
        return
    if total <= cap_bytes:
        return

    for candidate in index.eviction_candidates():
        if total <= cap_bytes:
            break
        path = Path(candidate.path)
        if not path.exists():
            index.mark_evicted(candidate.segment_id)
            continue
        try:
            size = path.stat().st_size
            path.unlink()
        except OSError:
            continue
        total -= size
        index.mark_evicted(candidate.segment_id)
        logger.debug(
            "evicted (tier-aware): %s (-%d bytes, motion=%s, uploaded=%s)",
            path.name, size, candidate.has_motion, candidate.uploaded_to_s3,
        )


async def _scan_once(
    directory: Path,
    state: _WatcherState,
    out: asyncio.Queue[NewSegment],
) -> None:
    import time

    try:
        entries = list(directory.iterdir())
    except OSError as e:
        logger.warning("failed to read segment dir: %s", e)
        return

    now_ms = int(time.time() * 1000)
    candidates: list[_Candidate] = []
    for entry in entries:
        if entry.suffix != ".ts" or entry.name in state.known:
            continue
        try:
            stat = entry.stat()
        except OSError:
            continue
        if stat.st_size == 0:
            continue
        mtime_ms = int(stat.st_mtime * 1000)
        if now_ms - mtime_ms < MTIME_QUIESCENCE_MS:
            continue
        if not _is_valid_ts(entry):
            logger.warning("skipping corrupt/partial segment: %s", entry.name)
            continue
        candidates.append(_Candidate(
            name=entry.name,
            path=entry,
            mtime_ms=mtime_ms,
            size=stat.st_size,
        ))

    candidates.sort(key=lambda c: c.name)
    for c in candidates:
        state.known.add(c.name)
        has_motion = state.detector.detect(c.path, c.size)
        seg = NewSegment(
            filename=c.name,
            path=c.path,
            start_ts=c.mtime_ms - SEGMENT_DURATION_SECS * 1000,
            end_ts=c.mtime_ms,
            size_bytes=c.size,
            has_motion=has_motion,
        )
        logger.debug(
            "new segment: %s (size=%d, motion=%s)",
            seg.filename, seg.size_bytes, has_motion,
        )
        try:
            await asyncio.wait_for(out.put(seg), QUEUE_PUT_TIMEOUT)
        except TimeoutError:
            logger.warning("segment queue full after %.0fs, dropping: %s",
                           QUEUE_PUT_TIMEOUT, seg.filename)
