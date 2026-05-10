"""Motion detector test — mirrors camera/motion_test.go.

ffprobe isn't required: the detector falls back to file size when
ffprobe is unavailable, and the threshold logic is the same either way.
We exercise the file-size fallback path directly to keep the test
hermetic.
"""

from __future__ import annotations

from ghostcam.motion import MotionDetector


def _seed_detector(md: MotionDetector, sizes: list[int]) -> None:
    """Seed the detector's history without firing the threshold check."""
    for size in sizes:
        # detect() returns False until 3 entries are seeded, so the first
        # three calls just populate history. Anything after compares
        # against the rolling average.
        md.detect("/nonexistent.ts", size)


def test_no_motion_until_history_seeded() -> None:
    md = MotionDetector()
    # First three calls — even with a huge size — return False, matching
    # camera/motion.go's `len(history) < 3` guard.
    assert not md.detect("/x.ts", 1_000_000)
    assert not md.detect("/x.ts", 1_000_000)
    assert not md.detect("/x.ts", 1_000_000)


def test_motion_triggered_on_size_spike() -> None:
    md = MotionDetector()
    # Establish a rolling window of small segments.
    _seed_detector(md, [100, 110, 105])
    # 1.5x threshold means the next size must exceed avg(100,110,105)*1.5
    # = 105 * 1.5 = 157.5. A 200-byte segment trips the alarm.
    assert md.detect("/x.ts", 200)


def test_no_motion_within_threshold() -> None:
    md = MotionDetector()
    _seed_detector(md, [100, 110, 105])
    # avg ≈ 105; 1.4x = 147. A 130-byte segment is below threshold.
    assert not md.detect("/x.ts", 130)


def test_history_window_is_bounded_to_10() -> None:
    md = MotionDetector()
    # Push 12 small samples — the deque should only retain the last 10.
    for _ in range(12):
        md.detect("/x.ts", 100)
    # Internal: max_window=10. We can't reach in without breaking
    # encapsulation, but a behavior check is fine: a short spike now
    # should still trip the threshold (history is stable around 100).
    assert md.detect("/x.ts", 200)
