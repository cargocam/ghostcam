"""S3 upload loop + pending-confirm persistence.

Drains the segment queue, requests presigned URLs in batches of 3,
PUTs segments to S3, and queues confirmations to piggy-back on the
next presign call.

Two persistence backends:

  * `SegmentIndex` (preferred when wired in by main.py): SQLite store
    that tracks every segment the camera has produced, including
    local-only ones in lazy mode. Used for both pending-confirm
    flushing AND the lazy-mode `upload_segments` priority path.
  * `PendingConfirms` (legacy JSON fallback): in-place compatibility
    for tests/setups that don't wire a SegmentIndex. Identical
    persistence semantics to the original module — atomic write of
    `pending_confirms.json` so a crash between PUT and confirm doesn't
    orphan an uploaded S3 object.

Power-mode integration (when `power` is wired in):
  * `lazy` upload_mode: non-motion segments are NOT uploaded
    proactively. They go into the SegmentIndex as local-only and
    only upload when the server pushes an `upload_segments` command.
    Motion-flagged segments still upload immediately.
  * `priority` deque: command handler pushes segment IDs here when
    `upload_segments` arrives. The loop drains it before the regular
    queue.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import os
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from ghostcam.client import Client, S3UploadError
from ghostcam.power_mode import PowerModeState
from ghostcam.segment_index import SegmentIndex
from ghostcam.wire import PresignedUrl, UploadedSegment

if TYPE_CHECKING:
    from ghostcam.watcher import NewSegment

logger = logging.getLogger(__name__)

MAX_UPLOAD_RETRIES = 3
PENDING_FILE = "pending_confirms.json"


@dataclass
class _SharedFlags:
    """Globals expressed as instance state. Capture pipeline reads
    server_unreachable to pause; the watcher reads storage_capped to
    know not to push more segments.

    Bandwidth-savings counters track motion-gated upload effectiveness
    in the field. Camera reports them in telemetry; admin UI surfaces
    them as "N segments skipped this cycle, M uploaded". Resets are
    soft — process restart re-zeros them since they're observability,
    not contract.
    """

    server_unreachable: bool = False
    storage_capped: bool = False
    presign_fail_count: int = 0
    motion_segments_uploaded: int = 0
    motion_segments_skipped: int = 0
    # Cumulative retry count since boot (every retry of a presign or PUT
    # increments this — eventually-successful uploads still count their
    # retries here). Surfaced via telemetry.segment_upload_retries.
    upload_retries_total: int = 0
    # Sliding window of recent (close → PUT 200) latencies in ms, capped
    # at WINDOW. telemetry_poll computes p95 over this.
    upload_latency_ms_window: list[int] = field(default_factory=list)


# Width of the upload-latency sliding window. Chosen to cover ~5 minutes
# of constant-mode traffic (1 segment every 6 s → ~50 segments). Small
# enough that one slow burst doesn't pollute the p95 for an hour.
UPLOAD_LATENCY_WINDOW = 50


def record_upload_latency(latency_ms: int) -> None:
    """Append to the latency window, evicting oldest entries when full."""
    win = flags.upload_latency_ms_window
    win.append(latency_ms)
    if len(win) > UPLOAD_LATENCY_WINDOW:
        del win[: len(win) - UPLOAD_LATENCY_WINDOW]


# Module-level singleton — main.py and capture.py both observe these.
flags = _SharedFlags()


class PendingConfirms:
    """Atomic JSON-backed store of pending UploadedSegment confirms.

    Kept for the legacy path and as the test surface for upload retry
    behaviour. New deployments thread a SegmentIndex through instead.
    """

    def __init__(self, data_dir: Path) -> None:
        self._path = data_dir / PENDING_FILE

    def load(self) -> list[UploadedSegment]:
        try:
            text = self._path.read_text()
        except OSError:
            return []
        try:
            raw = json.loads(text)
        except json.JSONDecodeError as e:
            logger.warning("corrupt pending_confirms.json, discarding: %s", e)
            return []
        out: list[UploadedSegment] = []
        for item in raw:
            try:
                out.append(UploadedSegment.model_validate(item))
            except Exception:  # pydantic.ValidationError
                continue
        return out

    def save(self, confirms: list[UploadedSegment]) -> None:
        if not self._path.parent.exists():
            return
        tmp = self._path.with_suffix(self._path.suffix + ".tmp")
        data = [c.model_dump(by_alias=True, exclude_none=True) for c in confirms]
        try:
            tmp.write_text(json.dumps(data))
            os.replace(tmp, self._path)
            with contextlib.suppress(OSError):
                self._path.chmod(0o600)
        except OSError as e:
            logger.warning("failed to persist pending confirms: %s", e)


@dataclass
class _UploadState:
    available_urls: deque[PresignedUrl] = field(default_factory=deque)
    confirms: list[UploadedSegment] = field(default_factory=list)


async def run_upload_loop(
    client: Client,
    data_dir: Path,
    segments: asyncio.Queue[NewSegment],
    *,
    index: SegmentIndex | None = None,
    power: PowerModeState | None = None,
    priority: deque[str] | None = None,
    recording_mode: str = "constant",
    get_recording_mode: Callable[[], str] | None = None,
) -> None:
    """Long-running upload loop.

    Optional kwargs are wired in by main.py for power-mode-aware
    behaviour. Tests / partial setups that pass only positional args
    get the legacy JSON-backed proactive-upload behaviour for
    backward compatibility with the original signature.

    `recording_mode` defaults to `"constant"` to preserve legacy
    behaviour. When set to `"motion"`, non-motion segments are
    deferred to the local-manifest path just like lazy upload mode —
    closes GH #75. The two ways to skip a non-motion segment
    (recording_mode=motion OR upload_mode=lazy) are equivalent at the
    upload-decision boundary; recording_mode is the higher-level user
    intent ("only record motion"), upload_mode is the lower-level
    bandwidth knob ("defer non-motion until scrubbed-to").

    `get_recording_mode` is an optional getter for live mode changes:
    when provided, every loop iteration re-reads the current mode so
    that constant ↔ motion transitions take effect without a daemon
    restart. The static `recording_mode` kwarg is the initial value
    used when no getter is wired in.
    """
    state = _UploadState()
    pending: PendingConfirms | None = None
    if index is None:
        pending = PendingConfirms(data_dir)
        state.confirms = pending.load()
    else:
        # SegmentIndex is the source of truth: pending confirms are the
        # uploaded-but-unconfirmed rows.
        state.confirms = index.pending_confirms()
    if state.confirms:
        logger.info("resuming %d pending upload confirmations", len(state.confirms))

    retry: deque[NewSegment] = deque()

    try:
        while True:
            # 1. Priority: server-requested uploads (lazy-mode scrub
            #    fulfilment). Drained before regular segments.
            if priority and index is not None:
                seg_id = priority.popleft()
                row = index.get(seg_id)
                if row is None:
                    logger.warning("upload_segments: unknown id %s", seg_id)
                    continue
                if row.uploaded_to_s3 or row.evicted:
                    continue
                from ghostcam.watcher import NewSegment as _NewSegment
                seg_obj = _NewSegment(
                    filename=Path(row.path).name,
                    path=Path(row.path),
                    start_ts=row.start_ts,
                    end_ts=row.end_ts,
                    size_bytes=row.size_bytes,
                    has_motion=row.has_motion,
                )
                if (failed := await _upload_with_retry(
                    client, data_dir, seg_obj, state, pending, index,
                )) is not None:
                    retry.append(failed)
                continue

            # 2. Retry queue: failed uploads waiting on backoff.
            if retry:
                seg = retry.popleft()
                backoff = (1 << seg.retry_count) * 2  # 2, 4, 8 s
                await asyncio.sleep(backoff)
                if (failed := await _upload_with_retry(
                    client, data_dir, seg, state, pending, index,
                )) is not None:
                    retry.append(failed)
                continue

            # 3. Regular new-segment queue.
            seg = await segments.get()

            # Index every segment regardless of upload decision so the
            # server can later request it via `upload_segments`.
            if index is not None:
                index.record_local(
                    segment_id=Path(seg.filename).stem,
                    path=str(seg.path),
                    start_ts=seg.start_ts,
                    end_ts=seg.end_ts,
                    size_bytes=seg.size_bytes,
                    has_motion=seg.has_motion,
                )

            # Two reasons to defer a non-motion segment to the local-
            # manifest path rather than upload it now:
            #   1. upload_mode = lazy (the lower-level bandwidth knob)
            #   2. recording_mode = motion (the higher-level user intent;
            #      see GH #75 — what "motion mode" was always supposed to
            #      mean for the upload path)
            # Either gate produces the same wire behaviour: segment
            # registered with uploaded_to_s3=FALSE; viewer scrub → camera
            # uploads on demand.
            current_recording_mode = (
                get_recording_mode() if get_recording_mode is not None else recording_mode
            )
            lazy_skip = (
                power is not None and not power.should_upload(seg.has_motion)
            )
            motion_mode_skip = (
                current_recording_mode == "motion" and not seg.has_motion
            )
            if lazy_skip or motion_mode_skip:
                reason = "lazy" if lazy_skip else "motion-only"
                logger.debug(
                    "%s mode: registering local-only segment %s",
                    reason, seg.filename,
                )
                flags.motion_segments_skipped += 1
                try:
                    await client.post_local_manifest([UploadedSegment(
                        segment_id=Path(seg.filename).stem,
                        start_ts=seg.start_ts,
                        end_ts=seg.end_ts,
                        size_bytes=seg.size_bytes,
                        has_motion=seg.has_motion,
                    )])
                except Exception as e:  # noqa: BLE001
                    logger.debug("local-manifest post failed: %s", e)
                continue
            if seg.has_motion:
                flags.motion_segments_uploaded += 1

            if (failed := await _upload_with_retry(
                client, data_dir, seg, state, pending, index,
            )) is not None:
                retry.append(failed)
    except asyncio.CancelledError:
        if state.confirms:
            try:
                await asyncio.wait_for(
                    client.request_presigned_urls(0, state.confirms),
                    5.0,
                )
                if pending is not None:
                    pending.save([])
                if index is not None:
                    index.mark_confirmed([c.segment_id for c in state.confirms])
                logger.info(
                    "flushed %d pending confirms on shutdown", len(state.confirms),
                )
            except (TimeoutError, Exception) as e:
                logger.warning(
                    "failed to flush pending confirms on shutdown: %s", e,
                )
        raise


async def _upload_with_retry(
    client: Client,
    data_dir: Path,
    seg: NewSegment,
    state: _UploadState,
    pending: PendingConfirms | None,
    index: SegmentIndex | None,
) -> NewSegment | None:
    ok = await _upload_segment(client, data_dir, seg, state, pending, index)
    if ok:
        return None
    if seg.retry_count >= MAX_UPLOAD_RETRIES:
        logger.error(
            "S3 upload failed after %d retries, giving up (segment stays on disk): %s",
            seg.retry_count, seg.filename,
        )
        return None
    seg.retry_count += 1
    flags.upload_retries_total += 1
    logger.warning(
        "S3 upload failed, will retry: %s (attempt %d/%d)",
        seg.filename, seg.retry_count, MAX_UPLOAD_RETRIES,
    )
    return seg


async def _upload_segment(
    client: Client,
    data_dir: Path,
    seg: NewSegment,
    state: _UploadState,
    pending: PendingConfirms | None,
    index: SegmentIndex | None,
) -> bool:
    if not state.available_urls:
        try:
            await _replenish_urls(client, data_dir, state, pending, index)
        except Exception as e:
            logger.warning("failed to get presigned URLs: %s", e)
            return False

    if flags.storage_capped:
        logger.debug("storage capped, keeping segment locally: %s", seg.filename)
        return True  # not retriable

    if not state.available_urls:
        logger.warning("no presigned URLs available, skipping: %s", seg.filename)
        return False

    presigned = state.available_urls.popleft()

    try:
        data = seg.path.read_bytes()
    except OSError as e:
        logger.warning("failed to read segment file %s: %s", seg.filename, e)
        return True  # gone, no retry

    # Latency is wall-time from "we have the data + a presigned URL" to
    # "S3 returned 2xx". Excludes presign latency intentionally —
    # presign p95 is a separate question that lives on the server side.
    upload_start = time.monotonic()
    try:
        await client.upload_segment(presigned.put_url, data)
    except S3UploadError as e:
        logger.warning("S3 upload failed: %s (%d)", seg.filename, e.status_code)
        if e.is_client_error:
            state.available_urls.clear()
        return False
    except Exception as e:
        logger.warning("S3 upload failed: %s (%s)", seg.filename, e)
        return False

    record_upload_latency(int((time.monotonic() - upload_start) * 1000))
    logger.debug("segment uploaded: %s", presigned.segment_id)
    state.confirms.append(UploadedSegment(
        segment_id=presigned.segment_id,
        start_ts=seg.start_ts,
        end_ts=seg.end_ts,
        size_bytes=seg.size_bytes,
        has_motion=seg.has_motion,
    ))
    if index is not None:
        index.mark_uploaded(presigned.segment_id)
    if pending is not None:
        pending.save(state.confirms)

    with contextlib.suppress(OSError):
        seg.path.unlink()
    return True


async def _replenish_urls(
    client: Client,
    data_dir: Path,
    state: _UploadState,
    pending: PendingConfirms | None,
    index: SegmentIndex | None,
) -> None:
    pending_confirms = state.confirms
    state.confirms = []

    try:
        resp = await client.request_presigned_urls(3, pending_confirms)
    except Exception:
        state.confirms = pending_confirms
        flags.presign_fail_count += 1
        if flags.presign_fail_count >= 3:
            flags.server_unreachable = True
        raise

    flags.presign_fail_count = 0
    if flags.server_unreachable:
        logger.info("server reachable again, resuming capture")
        flags.server_unreachable = False

    if pending_confirms:
        if pending is not None:
            pending.save([])
        if index is not None:
            index.mark_confirmed([c.segment_id for c in pending_confirms])

    if resp.storage_capped:
        if not flags.storage_capped:
            logger.warning("storage capped by server, pausing uploads")
        flags.storage_capped = True
        return

    if flags.storage_capped and resp.urls:
        logger.info("storage cap cleared, resuming uploads")
        flags.storage_capped = False

    for url in resp.urls:
        state.available_urls.append(url)
