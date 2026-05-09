"""Motion detection by P-frame size deltas.

Mirrors camera/motion.go. ffprobe is run on each finished segment;
P-frame packet sizes are averaged and compared to a rolling 10-segment
window. A 1.5x spike vs the rolling average flags motion. If ffprobe
isn't on PATH, we fall back to raw file-size comparison — same fallback
the Go camera uses.

ffprobe is one subprocess per 6 s segment, so the cost is negligible —
no need for asyncio plumbing here. We use the blocking subprocess from a
worker thread when needed (the watcher calls detect() synchronously).
"""

from __future__ import annotations

import logging
import shutil
import subprocess
from collections import deque
from pathlib import Path

logger = logging.getLogger(__name__)


class MotionDetector:
    """Rolling-window detector. Same constants as camera/motion.go."""

    def __init__(self, *, max_window: int = 10, threshold: float = 1.5) -> None:
        self._history: deque[float] = deque(maxlen=max_window)
        self._threshold = threshold
        self._ffprobe_avail: bool | None = None

    def detect(self, path: Path | str, file_size_bytes: int) -> bool:
        """Return True if the segment shows motion vs recent history."""
        avg = self._avg_pframe_size(Path(path))
        if avg <= 0:
            avg = float(file_size_bytes)

        if len(self._history) < 3:
            self._history.append(avg)
            return False

        rolling = sum(self._history) / len(self._history)
        has_motion = avg > rolling * self._threshold
        self._history.append(avg)
        return has_motion

    # --- internals ---

    def _avg_pframe_size(self, path: Path) -> float:
        if self._ffprobe_avail is False:
            return 0.0
        if shutil.which("ffprobe") is None:
            if self._ffprobe_avail is None:
                logger.debug("ffprobe not on PATH, using file-size fallback")
            self._ffprobe_avail = False
            return 0.0
        self._ffprobe_avail = True

        try:
            result = subprocess.run(
                [
                    "ffprobe", "-v", "quiet",
                    "-select_streams", "v",
                    "-show_entries", "frame=pict_type,pkt_size",
                    "-of", "csv=p=0",
                    str(path),
                ],
                capture_output=True,
                text=True,
                check=False,
            )
        except OSError:
            self._ffprobe_avail = False
            return 0.0

        total = 0
        count = 0
        for line in result.stdout.splitlines():
            line = line.rstrip(",")
            parts = line.split(",", 1)
            if len(parts) < 2 or parts[1] != "P":
                continue
            try:
                total += int(parts[0])
                count += 1
            except ValueError:
                continue
        return total / count if count else 0.0
