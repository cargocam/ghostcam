"""GH #75: recording_mode='motion' actually skips non-motion uploads.

The wire-level behaviour mirrors upload_mode='lazy' — non-motion
segments are deferred to the local-manifest path so the server's
timeline still shows the gap, but the camera doesn't burn cellular
bandwidth on static-scene footage.

The two test-relevant invariants this file pins:

  1. recording_mode='motion' + has_motion=False → no S3 upload,
     post_local_manifest called instead.
  2. recording_mode='motion' + has_motion=True → S3 upload as
     normal.
  3. recording_mode='constant' (today's default) is unaffected.
  4. flags.motion_segments_uploaded / motion_segments_skipped
     counters increment correctly so the field can measure
     bandwidth savings.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from pathlib import Path

import pytest

from ghostcam.upload import flags, run_upload_loop
from ghostcam.watcher import NewSegment
from ghostcam.wire import PresignedUrl, PresignResponse, UploadedSegment


@dataclass
class FakeClient:
    """Minimal client that captures upload + local-manifest calls."""

    presign_responses: list[PresignResponse] = field(default_factory=list)
    presign_calls: list[tuple[int, list[UploadedSegment]]] = field(default_factory=list)
    upload_paths: list[str] = field(default_factory=list)
    local_manifests: list[list[UploadedSegment]] = field(default_factory=list)

    async def request_presigned_urls(
        self,
        count: int,
        uploaded: list[UploadedSegment] | None = None,
    ) -> PresignResponse:
        self.presign_calls.append((count, list(uploaded or [])))
        if self.presign_responses:
            return self.presign_responses.pop(0)
        return PresignResponse(urls=[])

    async def upload_segment(self, presigned_url: str, data: bytes) -> None:
        self.upload_paths.append(presigned_url)

    async def post_local_manifest(self, segments: list[UploadedSegment]) -> None:
        self.local_manifests.append(list(segments))


def _make_segment(tmp_path: Path, name: str, has_motion: bool) -> NewSegment:
    p = tmp_path / name
    p.write_bytes(b"\x47" + b"\x00" * 1023)
    return NewSegment(
        filename=name,
        path=p,
        start_ts=1_000,
        end_ts=2_000,
        size_bytes=1024,
        has_motion=has_motion,
    )


def _presigned(seg_id: str) -> PresignedUrl:
    return PresignedUrl(
        segment_id=seg_id,
        s3_key=f"{seg_id}.ts",
        put_url=f"https://s3.example/{seg_id}",
        expires_at=10**12,
    )


@pytest.fixture(autouse=True)
def _reset_flags() -> AsyncIterator[None]:
    flags.server_unreachable = False
    flags.storage_capped = False
    flags.presign_fail_count = 0
    flags.motion_segments_uploaded = 0
    flags.motion_segments_skipped = 0
    yield


@contextlib.asynccontextmanager
async def _drive(
    fake: FakeClient,
    seg: NewSegment,
    tmp_path: Path,
    *,
    recording_mode: str,
) -> AsyncIterator[None]:
    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    await queue.put(seg)
    task = asyncio.create_task(
        run_upload_loop(
            fake,  # type: ignore[arg-type]
            tmp_path,
            queue,
            recording_mode=recording_mode,
        ),
    )
    try:
        await asyncio.sleep(0.05)
        yield
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task


@pytest.mark.asyncio
async def test_motion_mode_skips_non_motion_upload(tmp_path: Path) -> None:
    fake = FakeClient()
    seg = _make_segment(tmp_path, "seg00001.ts", has_motion=False)
    async with _drive(fake, seg, tmp_path, recording_mode="motion"):
        pass
    # No S3 upload happened.
    assert fake.upload_paths == []
    # Local manifest got the segment.
    assert len(fake.local_manifests) == 1
    assert fake.local_manifests[0][0].segment_id == "seg00001"
    # File untouched on disk (we DIDN'T unlink it).
    assert seg.path.exists()
    # Counter incremented.
    assert flags.motion_segments_skipped == 1
    assert flags.motion_segments_uploaded == 0


@pytest.mark.asyncio
async def test_motion_mode_uploads_motion_segment(tmp_path: Path) -> None:
    fake = FakeClient(
        presign_responses=[PresignResponse(urls=[_presigned("seg-motion")])],
    )
    seg = _make_segment(tmp_path, "seg00002.ts", has_motion=True)
    async with _drive(fake, seg, tmp_path, recording_mode="motion"):
        pass
    # Upload happened.
    assert fake.upload_paths == ["https://s3.example/seg-motion"]
    # No local manifest entry.
    assert fake.local_manifests == []
    # File was unlinked after upload.
    assert not seg.path.exists()
    # Counter incremented.
    assert flags.motion_segments_uploaded == 1
    assert flags.motion_segments_skipped == 0


@pytest.mark.asyncio
async def test_constant_mode_uploads_everything(tmp_path: Path) -> None:
    """Regression: recording_mode='constant' (default) is unchanged.
    Both motion and non-motion segments upload."""
    fake = FakeClient(
        presign_responses=[PresignResponse(urls=[_presigned("seg-static")])],
    )
    seg = _make_segment(tmp_path, "seg00003.ts", has_motion=False)
    async with _drive(fake, seg, tmp_path, recording_mode="constant"):
        pass
    assert fake.upload_paths == ["https://s3.example/seg-static"]
    assert fake.local_manifests == []
    assert flags.motion_segments_skipped == 0


@pytest.mark.asyncio
async def test_default_recording_mode_is_constant(tmp_path: Path) -> None:
    """Legacy callers that don't pass recording_mode at all behave as
    constant. Preserves backward compatibility for existing test code."""
    fake = FakeClient(
        presign_responses=[PresignResponse(urls=[_presigned("seg-legacy")])],
    )
    seg = _make_segment(tmp_path, "seg00004.ts", has_motion=False)
    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    await queue.put(seg)
    task = asyncio.create_task(
        run_upload_loop(fake, tmp_path, queue),  # type: ignore[arg-type]
    )
    try:
        await asyncio.sleep(0.05)
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task
    assert fake.upload_paths == ["https://s3.example/seg-legacy"]
