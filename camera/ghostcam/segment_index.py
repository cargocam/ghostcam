"""SQLite-backed segment manifest.

Replaces (and extends) the old `pending_confirms.json` in two ways:

  1. It tracks every segment the camera has produced, not only the ones
     with a pending S3 confirm. That's load-bearing for lazy upload
     mode where most segments are local-only and we still need to tell
     the server about them so coverage bars exist in the UI.

  2. It supports time-range queries via a sorted `start_ts` index.
     `find_overlapping(start_ts, end_ts)` is the path the server hits
     (indirectly, via `upload_segments` commands) when a viewer scrubs
     to a region that hasn't been uploaded yet.

Schema:

    segments(
        segment_id     TEXT PRIMARY KEY,
        path           TEXT NOT NULL,
        start_ts       INTEGER NOT NULL,    -- Unix milliseconds
        end_ts         INTEGER NOT NULL,
        size_bytes     INTEGER NOT NULL,
        has_motion     INTEGER NOT NULL,    -- 0/1
        uploaded_to_s3 INTEGER NOT NULL,    -- 0=local-only, 1=at-S3
        confirmed      INTEGER NOT NULL,    -- 0=server hasn't acked yet, 1=acked
        evicted        INTEGER NOT NULL,    -- 0=on disk, 1=local file gone
        created_ms     INTEGER NOT NULL     -- when we first saw it (Unix ms)
    )

Confirms-flush behaviour mirrors `pending_confirms.json`: the upload
loop appends `(segment_id, …, uploaded_to_s3=1, confirmed=0)` after a
successful S3 PUT, then on the next presign call it sends those rows
as `uploaded` to the server. When the server accepts, we mark them
confirmed.

Eviction policy used by the watcher's storage cap:
   uploaded&confirmed → unuploaded&!motion → unuploaded&motion (last)
"""

from __future__ import annotations

import contextlib
import logging
import sqlite3
import time
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

from ghostcam.wire import UploadedSegment

logger = logging.getLogger(__name__)

DB_FILENAME = "segments.sqlite"


@dataclass(frozen=True, slots=True)
class SegmentRow:
    """One row of the segments table, exposed to callers as an immutable
    snapshot."""

    segment_id: str
    path: str
    start_ts: int
    end_ts: int
    size_bytes: int
    has_motion: bool
    uploaded_to_s3: bool
    confirmed: bool
    evicted: bool
    created_ms: int


_SCHEMA = """
CREATE TABLE IF NOT EXISTS segments (
    segment_id     TEXT PRIMARY KEY,
    path           TEXT NOT NULL,
    start_ts       INTEGER NOT NULL,
    end_ts         INTEGER NOT NULL,
    size_bytes     INTEGER NOT NULL,
    has_motion     INTEGER NOT NULL,
    uploaded_to_s3 INTEGER NOT NULL,
    confirmed      INTEGER NOT NULL,
    evicted        INTEGER NOT NULL,
    created_ms     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_segments_time
    ON segments(start_ts, end_ts);
CREATE INDEX IF NOT EXISTS idx_segments_state
    ON segments(uploaded_to_s3, confirmed, evicted);
"""


class SegmentIndex:
    """Thin wrapper around sqlite3 with the segment-specific operations
    the watcher / upload loop / lazy-upload command handler need."""

    def __init__(self, data_dir: Path) -> None:
        data_dir.mkdir(parents=True, exist_ok=True)
        self._path = data_dir / DB_FILENAME
        self._conn = sqlite3.connect(
            str(self._path),
            isolation_level=None,  # autocommit; we use explicit BEGIN/COMMIT
            timeout=10.0,
        )
        self._conn.executescript("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;")
        self._conn.executescript(_SCHEMA)
        self._conn.row_factory = sqlite3.Row

    def close(self) -> None:
        with contextlib.suppress(sqlite3.Error):
            self._conn.close()

    def __enter__(self) -> SegmentIndex:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    # --- mutations ---

    def record_local(
        self,
        *,
        segment_id: str,
        path: str,
        start_ts: int,
        end_ts: int,
        size_bytes: int,
        has_motion: bool,
    ) -> None:
        """Watcher path: a freshly-finished local segment exists. Idempotent
        on segment_id so re-detection of the same file (e.g. on restart)
        doesn't duplicate."""
        self._conn.execute(
            """
            INSERT INTO segments
                (segment_id, path, start_ts, end_ts, size_bytes, has_motion,
                 uploaded_to_s3, confirmed, evicted, created_ms)
            VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?)
            ON CONFLICT(segment_id) DO NOTHING
            """,
            (
                segment_id,
                path,
                start_ts,
                end_ts,
                size_bytes,
                int(has_motion),
                int(time.time() * 1000),
            ),
        )

    def mark_uploaded(self, segment_id: str) -> None:
        """Upload loop: PUT to S3 succeeded; awaiting server confirm."""
        self._conn.execute(
            "UPDATE segments SET uploaded_to_s3 = 1 WHERE segment_id = ?",
            (segment_id,),
        )

    def mark_confirmed(self, segment_ids: list[str]) -> None:
        """Upload loop: server accepted these confirms on a presign call."""
        if not segment_ids:
            return
        with self._txn():
            self._conn.executemany(
                "UPDATE segments SET confirmed = 1 WHERE segment_id = ?",
                [(sid,) for sid in segment_ids],
            )

    def mark_evicted(self, segment_id: str) -> None:
        """Storage-cap path: local .ts file deleted to free disk."""
        self._conn.execute(
            "UPDATE segments SET evicted = 1 WHERE segment_id = ?",
            (segment_id,),
        )

    def delete(self, segment_id: str) -> None:
        """Hard delete from the index. Used after the segment is both
        confirmed AND evicted, so we don't carry forever."""
        self._conn.execute(
            "DELETE FROM segments WHERE segment_id = ?",
            (segment_id,),
        )

    # --- reads ---

    def get(self, segment_id: str) -> SegmentRow | None:
        cur = self._conn.execute(
            "SELECT * FROM segments WHERE segment_id = ?",
            (segment_id,),
        )
        row = cur.fetchone()
        return _row_to_dataclass(row) if row else None

    def pending_confirms(self) -> list[UploadedSegment]:
        """Mirrors the old `pending_confirms.json::load`: rows that are
        uploaded to S3 but not yet acked by the server. Used by upload.py
        to piggy-back confirmations on the next presign call."""
        cur = self._conn.execute(
            """
            SELECT segment_id, start_ts, end_ts, size_bytes, has_motion
            FROM segments
            WHERE uploaded_to_s3 = 1 AND confirmed = 0
            ORDER BY start_ts ASC
            """,
        )
        return [
            UploadedSegment(
                segment_id=r["segment_id"],
                start_ts=r["start_ts"],
                end_ts=r["end_ts"],
                size_bytes=r["size_bytes"],
                has_motion=bool(r["has_motion"]),
            )
            for r in cur.fetchall()
        ]

    def find_overlapping(
        self, start_ts: int, end_ts: int, *, only_local: bool = False
    ) -> list[SegmentRow]:
        """Segments whose [start_ts, end_ts] intersects [start_ts, end_ts].

        `only_local=True` restricts to rows the camera could still upload
        — i.e. uploaded_to_s3=0 AND evicted=0. That's what the lazy-upload
        command handler queries when the server asks for specific segment
        IDs and we want to verify they're still here.
        """
        sql = (
            "SELECT * FROM segments "
            "WHERE start_ts < ? AND end_ts > ? "  # interval overlap test
        )
        params: list[object] = [end_ts, start_ts]
        if only_local:
            sql += "AND uploaded_to_s3 = 0 AND evicted = 0 "
        sql += "ORDER BY start_ts ASC"
        cur = self._conn.execute(sql, params)
        return [_row_to_dataclass(r) for r in cur.fetchall()]

    def eviction_candidates(self) -> Iterator[SegmentRow]:
        """Yield rows in eviction priority order:

          1. uploaded_to_s3 AND confirmed (safe — copy at S3)
          2. uploaded_to_s3 AND NOT confirmed (server will re-confirm)
          3. NOT uploaded AND NOT motion (lazy-mode local-only, low value)
          4. NOT uploaded AND motion (last — losing motion segments
             defeats the alerting use case)

        Within each tier, oldest start_ts first.
        """
        for sql in (
            "uploaded_to_s3 = 1 AND confirmed = 1 AND evicted = 0",
            "uploaded_to_s3 = 1 AND confirmed = 0 AND evicted = 0",
            "uploaded_to_s3 = 0 AND has_motion = 0 AND evicted = 0",
            "uploaded_to_s3 = 0 AND has_motion = 1 AND evicted = 0",
        ):
            cur = self._conn.execute(
                f"SELECT * FROM segments WHERE {sql} ORDER BY start_ts ASC",
            )
            for row in cur.fetchall():
                yield _row_to_dataclass(row)

    def known_filenames(self) -> set[str]:
        """Watcher uses this on boot to seed its `known` set so it doesn't
        re-process segments we already indexed last run."""
        cur = self._conn.execute(
            "SELECT path FROM segments WHERE evicted = 0",
        )
        return {Path(r["path"]).name for r in cur.fetchall()}

    def stats(self) -> dict[str, int]:
        """Quick counters for telemetry / debugging."""
        cur = self._conn.execute(
            """
            SELECT
                COUNT(*) AS total,
                SUM(CASE WHEN uploaded_to_s3 = 1 THEN 1 ELSE 0 END) AS uploaded,
                SUM(CASE WHEN uploaded_to_s3 = 0 AND evicted = 0 THEN 1 ELSE 0 END) AS pending_upload,
                SUM(CASE WHEN evicted = 1 THEN 1 ELSE 0 END) AS evicted,
                SUM(CASE WHEN has_motion = 1 THEN 1 ELSE 0 END) AS motion
            FROM segments
            """,
        )
        row = cur.fetchone()
        return {
            "total": row["total"] or 0,
            "uploaded": row["uploaded"] or 0,
            "pending_upload": row["pending_upload"] or 0,
            "evicted": row["evicted"] or 0,
            "motion": row["motion"] or 0,
        }

    # --- internals ---

    @contextlib.contextmanager
    def _txn(self) -> Iterator[None]:
        self._conn.execute("BEGIN")
        try:
            yield
        except BaseException:
            self._conn.execute("ROLLBACK")
            raise
        else:
            self._conn.execute("COMMIT")


def _row_to_dataclass(r: sqlite3.Row) -> SegmentRow:
    return SegmentRow(
        segment_id=r["segment_id"],
        path=r["path"],
        start_ts=r["start_ts"],
        end_ts=r["end_ts"],
        size_bytes=r["size_bytes"],
        has_motion=bool(r["has_motion"]),
        uploaded_to_s3=bool(r["uploaded_to_s3"]),
        confirmed=bool(r["confirmed"]),
        evicted=bool(r["evicted"]),
        created_ms=r["created_ms"],
    )


__all__ = [
    "DB_FILENAME",
    "SegmentIndex",
    "SegmentRow",
]
