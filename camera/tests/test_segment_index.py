"""SegmentIndex: CRUD + time-range queries + eviction priority order.

The index is load-bearing for lazy upload and for recovery from a
mid-upload crash, so we cover both paths."""

from __future__ import annotations

from pathlib import Path

import pytest

from ghostcam.segment_index import SegmentIndex


@pytest.fixture
def idx(tmp_path: Path) -> SegmentIndex:
    return SegmentIndex(tmp_path)


def _seg(idx: SegmentIndex, segment_id: str, *, start_ts: int, end_ts: int,
         has_motion: bool = False, size_bytes: int = 1000) -> None:
    idx.record_local(
        segment_id=segment_id,
        path=f"/var/ghostcam/segments/{segment_id}.ts",
        start_ts=start_ts,
        end_ts=end_ts,
        size_bytes=size_bytes,
        has_motion=has_motion,
    )


def test_record_and_get(idx: SegmentIndex) -> None:
    _seg(idx, "seg1", start_ts=1000, end_ts=7000, has_motion=True)
    row = idx.get("seg1")
    assert row is not None
    assert row.segment_id == "seg1"
    assert row.start_ts == 1000
    assert row.end_ts == 7000
    assert row.has_motion is True
    assert row.uploaded_to_s3 is False
    assert row.confirmed is False
    assert row.evicted is False


def test_record_is_idempotent(idx: SegmentIndex) -> None:
    _seg(idx, "seg1", start_ts=1000, end_ts=7000)
    _seg(idx, "seg1", start_ts=2000, end_ts=8000)  # second insert ignored
    row = idx.get("seg1")
    assert row is not None
    assert row.start_ts == 1000  # original wins (ON CONFLICT DO NOTHING)


def test_mark_uploaded_then_confirmed(idx: SegmentIndex) -> None:
    _seg(idx, "seg1", start_ts=1000, end_ts=7000)
    idx.mark_uploaded("seg1")
    row = idx.get("seg1")
    assert row is not None
    assert row.uploaded_to_s3 is True
    assert row.confirmed is False

    idx.mark_confirmed(["seg1"])
    row = idx.get("seg1")
    assert row is not None
    assert row.confirmed is True


def test_pending_confirms_returns_uploaded_unconfirmed(idx: SegmentIndex) -> None:
    """The upload loop's confirms-flush path: rows that hit S3 but the
    server hasn't acked yet."""
    _seg(idx, "a", start_ts=1000, end_ts=7000)
    _seg(idx, "b", start_ts=7000, end_ts=13000)
    _seg(idx, "c", start_ts=13000, end_ts=19000)
    idx.mark_uploaded("a")
    idx.mark_uploaded("b")
    idx.mark_uploaded("c")
    idx.mark_confirmed(["b"])

    pending = idx.pending_confirms()
    assert {p.segment_id for p in pending} == {"a", "c"}


def test_pending_confirms_excludes_local_only(idx: SegmentIndex) -> None:
    """Lazy-mode local-only segments must NOT appear as pending confirms."""
    _seg(idx, "lazy", start_ts=1000, end_ts=7000)  # never uploaded
    pending = idx.pending_confirms()
    assert pending == []


def test_find_overlapping_basic(idx: SegmentIndex) -> None:
    _seg(idx, "a", start_ts=1000, end_ts=7000)
    _seg(idx, "b", start_ts=7000, end_ts=13000)
    _seg(idx, "c", start_ts=13000, end_ts=19000)

    # Range that hits the middle two.
    rows = idx.find_overlapping(6000, 14000)
    assert {r.segment_id for r in rows} == {"a", "b", "c"}

    # Strict miss.
    rows = idx.find_overlapping(20000, 30000)
    assert rows == []


def test_find_overlapping_only_local(idx: SegmentIndex) -> None:
    """`only_local=True` filters to rows the camera could still upload."""
    _seg(idx, "uploaded", start_ts=1000, end_ts=7000)
    _seg(idx, "local-only", start_ts=7000, end_ts=13000)
    idx.mark_uploaded("uploaded")

    rows = idx.find_overlapping(0, 20000, only_local=True)
    assert {r.segment_id for r in rows} == {"local-only"}


def test_find_overlapping_excludes_evicted(idx: SegmentIndex) -> None:
    _seg(idx, "gone", start_ts=1000, end_ts=7000)
    idx.mark_evicted("gone")
    rows = idx.find_overlapping(0, 20000, only_local=True)
    assert rows == []


def test_eviction_candidate_ordering(idx: SegmentIndex) -> None:
    """Tier 1 (uploaded+confirmed) → Tier 2 (uploaded but not confirmed)
    → Tier 3 (local-only no motion) → Tier 4 (local-only motion).
    Within each tier, oldest start_ts first."""
    _seg(idx, "t4-old", start_ts=1000, end_ts=7000, has_motion=True)
    _seg(idx, "t4-new", start_ts=7000, end_ts=13000, has_motion=True)
    _seg(idx, "t3-old", start_ts=13000, end_ts=19000)
    _seg(idx, "t3-new", start_ts=19000, end_ts=25000)
    _seg(idx, "t2", start_ts=25000, end_ts=31000)
    idx.mark_uploaded("t2")
    _seg(idx, "t1-old", start_ts=31000, end_ts=37000)
    _seg(idx, "t1-new", start_ts=37000, end_ts=43000)
    idx.mark_uploaded("t1-old")
    idx.mark_uploaded("t1-new")
    idx.mark_confirmed(["t1-old", "t1-new"])

    order = [c.segment_id for c in idx.eviction_candidates()]
    assert order == [
        "t1-old", "t1-new",  # confirmed uploads first (safest to drop)
        "t2",                # uploaded but unconfirmed
        "t3-old", "t3-new",  # local-only no motion
        "t4-old", "t4-new",  # local-only motion (last resort)
    ]


def test_eviction_candidates_skips_already_evicted(idx: SegmentIndex) -> None:
    _seg(idx, "a", start_ts=1000, end_ts=7000)
    idx.mark_evicted("a")
    assert list(idx.eviction_candidates()) == []


def test_known_filenames(idx: SegmentIndex) -> None:
    _seg(idx, "seg00001", start_ts=1000, end_ts=7000)
    _seg(idx, "seg00002", start_ts=7000, end_ts=13000)
    idx.mark_evicted("seg00001")  # excluded
    assert idx.known_filenames() == {"seg00002.ts"}


def test_stats_counters(idx: SegmentIndex) -> None:
    _seg(idx, "a", start_ts=1000, end_ts=7000, has_motion=True)
    _seg(idx, "b", start_ts=7000, end_ts=13000)
    _seg(idx, "c", start_ts=13000, end_ts=19000)
    idx.mark_uploaded("a")
    idx.mark_evicted("c")

    stats = idx.stats()
    assert stats == {
        "total": 3,
        "uploaded": 1,        # a
        "pending_upload": 1,  # b (c is evicted)
        "evicted": 1,         # c
        "motion": 1,          # a
    }


def test_persistence_across_open(tmp_path: Path) -> None:
    """The whole point of SQLite over JSON: state survives reopens."""
    idx1 = SegmentIndex(tmp_path)
    _seg(idx1, "persist", start_ts=1000, end_ts=7000)
    idx1.mark_uploaded("persist")
    idx1.close()

    idx2 = SegmentIndex(tmp_path)
    row = idx2.get("persist")
    assert row is not None
    assert row.uploaded_to_s3 is True
    idx2.close()


def test_delete_hard_removes(idx: SegmentIndex) -> None:
    _seg(idx, "old", start_ts=1000, end_ts=7000)
    idx.delete("old")
    assert idx.get("old") is None
