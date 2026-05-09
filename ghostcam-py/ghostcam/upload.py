"""S3 upload loop + pending-confirm persistence.

Mirrors camera/upload.go. Drains the segment queue, requests presigned
URLs in batches of 3, PUTs segments to S3, and queues confirmations to
piggy-back on the next presign call.

Pending confirmations are persisted atomically to
{data_dir}/pending_confirms.json so a crash between the S3 PUT and the
confirming presign request does NOT orphan an uploaded object — the
next process startup re-confirms.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from ghostcam.client import Client, S3UploadError
from ghostcam.wire import PresignedUrl, UploadedSegment

if TYPE_CHECKING:
    from ghostcam.watcher import NewSegment

logger = logging.getLogger(__name__)

MAX_UPLOAD_RETRIES = 3
PENDING_FILE = "pending_confirms.json"


@dataclass
class _SharedFlags:
    """Globals from camera/upload.go expressed as instance state. The
    capture pipeline reads server_unreachable to pause; the watcher
    reads storage_capped to know not to push more segments.
    """
    server_unreachable: bool = False
    storage_capped: bool = False
    presign_fail_count: int = 0


# Module-level singleton because main.py and capture.py both need to
# observe these. Mirrors how the Go camera uses package-level atomics.
flags = _SharedFlags()


class PendingConfirms:
    """Atomic JSON-backed store of pending UploadedSegment confirms."""

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
            try:
                self._path.chmod(0o600)
            except OSError:
                pass
        except OSError as e:
            logger.warning("failed to persist pending confirms: %s", e)


@dataclass
class _UploadState:
    available_urls: deque[PresignedUrl] = field(default_factory=deque)
    confirms: list[UploadedSegment] = field(default_factory=list)


async def run_upload_loop(
    client: Client,
    data_dir: Path,
    segments: asyncio.Queue["NewSegment"],
) -> None:
    """Consume segments, upload via presigned URLs, persist confirms.

    Cancellation flushes any pending confirms with a 5 s budget.
    """
    state = _UploadState()
    pending = PendingConfirms(data_dir)
    state.confirms = pending.load()
    if state.confirms:
        logger.info("resuming %d pending upload confirmations", len(state.confirms))

    retry: deque["NewSegment"] = deque()

    try:
        while True:
            if retry:
                seg = retry.popleft()
                backoff = (1 << seg.retry_count) * 2  # 2, 4, 8 s
                await asyncio.sleep(backoff)
                if (failed := await _upload_with_retry(
                    client, data_dir, seg, state, pending
                )) is not None:
                    retry.append(failed)
                continue

            seg = await segments.get()
            if (failed := await _upload_with_retry(
                client, data_dir, seg, state, pending
            )) is not None:
                retry.append(failed)
    except asyncio.CancelledError:
        if state.confirms:
            try:
                await asyncio.wait_for(
                    client.request_presigned_urls(0, state.confirms),
                    5.0,
                )
                pending.save([])
                logger.info("flushed %d pending confirms on shutdown", len(state.confirms))
            except (asyncio.TimeoutError, Exception) as e:
                logger.warning("failed to flush pending confirms on shutdown: %s", e)
        raise


async def _upload_with_retry(
    client: Client,
    data_dir: Path,
    seg: "NewSegment",
    state: _UploadState,
    pending: PendingConfirms,
) -> "NewSegment | None":
    ok = await _upload_segment(client, data_dir, seg, state, pending)
    if ok:
        return None
    if seg.retry_count >= MAX_UPLOAD_RETRIES:
        logger.error(
            "S3 upload failed after %d retries, giving up (segment stays on disk): %s",
            seg.retry_count, seg.filename,
        )
        return None
    seg.retry_count += 1
    logger.warning(
        "S3 upload failed, will retry: %s (attempt %d/%d)",
        seg.filename, seg.retry_count, MAX_UPLOAD_RETRIES,
    )
    return seg


async def _upload_segment(
    client: Client,
    data_dir: Path,
    seg: "NewSegment",
    state: _UploadState,
    pending: PendingConfirms,
) -> bool:
    if not state.available_urls:
        try:
            await _replenish_urls(client, data_dir, state, pending)
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

    try:
        await client.upload_segment(presigned.put_url, data)
    except S3UploadError as e:
        logger.warning("S3 upload failed: %s (%d)", seg.filename, e.status_code)
        if e.is_client_error:
            # 4xx means URL is expired/invalid; discard cache so next call
            # gets fresh URLs. Don't burn a retry.
            state.available_urls.clear()
        return False
    except Exception as e:
        logger.warning("S3 upload failed: %s (%s)", seg.filename, e)
        return False

    logger.debug("segment uploaded: %s", presigned.segment_id)
    state.confirms.append(UploadedSegment(
        segment_id=presigned.segment_id,
        start_ts=seg.start_ts,
        end_ts=seg.end_ts,
        size_bytes=seg.size_bytes,
        has_motion=seg.has_motion,
    ))
    pending.save(state.confirms)

    try:
        seg.path.unlink()
    except OSError:
        pass
    return True


async def _replenish_urls(
    client: Client,
    data_dir: Path,
    state: _UploadState,
    pending: PendingConfirms,
) -> None:
    pending_confirms = state.confirms
    state.confirms = []

    try:
        resp = await client.request_presigned_urls(3, pending_confirms)
    except Exception:
        # Restore confirms (on-disk copy is intact) so they aren't lost.
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
        pending.save([])

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
