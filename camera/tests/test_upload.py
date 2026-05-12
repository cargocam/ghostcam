"""Upload loop test — mirrors camera/upload_test.go.

We exercise the persistent confirm-queue ordering, the storage-capped
flag, and the 4xx-clears-URL-cache behavior using a fake Client. The
real Client makes HTTP calls; the fake records them so we can assert
ordering without sockets.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
from dataclasses import dataclass, field
from pathlib import Path

import pytest

from ghostcam import upload as upload_module
from ghostcam.client import S3UploadError
from ghostcam.upload import (
    PendingConfirms,
    flags,
    run_upload_loop,
)
from ghostcam.watcher import NewSegment
from ghostcam.wire import PresignedUrl, PresignResponse, UploadedSegment


@dataclass
class FakeClient:
    """Minimal Client stand-in. Records requests, scripts responses."""

    presign_responses: list[PresignResponse] = field(default_factory=list)
    presign_calls: list[tuple[int, list[UploadedSegment], list[UploadedSegment]]] = field(default_factory=list)
    upload_paths: list[str] = field(default_factory=list)
    s3_failure_status: int | None = None

    async def request_presigned_urls(
        self,
        count: int,
        uploaded: list[UploadedSegment] | None = None,
        pending: list[UploadedSegment] | None = None,
    ) -> PresignResponse:
        self.presign_calls.append((count, list(uploaded or []), list(pending or [])))
        if self.presign_responses:
            return self.presign_responses.pop(0)
        return PresignResponse(urls=[])

    async def upload_segment(self, presigned_url: str, data: bytes) -> None:
        self.upload_paths.append(presigned_url)
        if self.s3_failure_status is not None:
            raise S3UploadError(self.s3_failure_status)


def _make_segment(tmp_path: Path, name: str, content: bytes = b"x") -> NewSegment:
    p = tmp_path / name
    p.write_bytes(content)
    return NewSegment(
        filename=name,
        path=p,
        start_ts=1_000,
        end_ts=2_000,
        size_bytes=len(content),
        has_motion=False,
    )


def _presigned(seg_id: str, url: str = "https://s3.example/put") -> PresignedUrl:
    return PresignedUrl(
        segment_id=seg_id,
        s3_key=f"{seg_id}.ts",
        put_url=url,
        expires_at=10**12,
    )


@pytest.fixture(autouse=True)
def _reset_flags() -> None:
    flags.server_unreachable = False
    flags.storage_capped = False
    flags.presign_fail_count = 0


@pytest.mark.asyncio
async def test_pending_confirms_persist_atomically(tmp_path: Path) -> None:
    pc = PendingConfirms(tmp_path)
    pc.save([
        UploadedSegment(segment_id="abc", start_ts=1, end_ts=2, size_bytes=3, has_motion=False),
        UploadedSegment(segment_id="def", start_ts=4, end_ts=5, size_bytes=6, has_motion=True),
    ])
    raw = json.loads((tmp_path / "pending_confirms.json").read_text())
    assert {r["segment_id"] for r in raw} == {"abc", "def"}
    # No leftover .tmp file.
    assert not (tmp_path / "pending_confirms.json.tmp").exists()


@pytest.mark.asyncio
async def test_pending_confirms_load_handles_corruption(tmp_path: Path) -> None:
    (tmp_path / "pending_confirms.json").write_text("not-valid-json")
    pc = PendingConfirms(tmp_path)
    assert pc.load() == []


@pytest.mark.asyncio
async def test_storage_capped_keeps_segments_locally(tmp_path: Path) -> None:
    fake = FakeClient(presign_responses=[
        PresignResponse(urls=[], storage_capped=True),
    ])
    seg = _make_segment(tmp_path, "seg00001.ts")
    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    await queue.put(seg)

    task = asyncio.create_task(run_upload_loop(fake, tmp_path, queue))  # type: ignore[arg-type]
    await asyncio.sleep(0.05)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    assert flags.storage_capped is True
    # Segment still on disk; nothing was uploaded.
    assert seg.path.exists()
    assert fake.upload_paths == []


@pytest.mark.asyncio
async def test_4xx_clears_url_cache(tmp_path: Path) -> None:
    # First presign returns 1 URL; segment fails with 403; second
    # presign returns another URL and the retry succeeds.
    fake = FakeClient(
        presign_responses=[
            PresignResponse(urls=[_presigned("seg-1")]),
            PresignResponse(urls=[_presigned("seg-1-retry", "https://s3.example/retry")]),
        ],
    )
    fake.s3_failure_status = 403
    seg = _make_segment(tmp_path, "seg00001.ts")
    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    await queue.put(seg)

    task = asyncio.create_task(run_upload_loop(fake, tmp_path, queue))  # type: ignore[arg-type]
    await asyncio.sleep(0.02)
    fake.s3_failure_status = None  # let retry succeed
    # First-retry backoff is `1<<1 * 2` = 4 s (matches camera/upload.go).
    # Wait long enough for the retry attempt to actually fire.
    await asyncio.sleep(4.5)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    # First presign call (count=3, no confirms), then a second presign
    # call after the 4xx wiped the URL cache.
    assert len(fake.presign_calls) >= 2
    # Two upload attempts: one failed with 403, retry succeeded.
    assert len(fake.upload_paths) == 2
    # Successful retry deleted the local file.
    assert not seg.path.exists()


@pytest.mark.asyncio
async def test_resumes_pending_confirms_on_startup(tmp_path: Path) -> None:
    PendingConfirms(tmp_path).save([
        UploadedSegment(segment_id="prev-1", start_ts=1, end_ts=2, size_bytes=3),
    ])
    fake = FakeClient(
        presign_responses=[PresignResponse(urls=[_presigned("seg-new")])],
    )
    seg = _make_segment(tmp_path, "seg00001.ts")
    queue: asyncio.Queue[NewSegment] = asyncio.Queue()
    await queue.put(seg)

    task = asyncio.create_task(run_upload_loop(fake, tmp_path, queue))  # type: ignore[arg-type]
    await asyncio.sleep(0.05)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    # The first presign call should carry the previously-pending confirm.
    assert fake.presign_calls
    first_count, first_uploaded, _first_pending = fake.presign_calls[0]
    assert any(u.segment_id == "prev-1" for u in first_uploaded)


def test_upload_latency_window_caps_at_window_size() -> None:
    flags.upload_latency_ms_window.clear()
    for i in range(200):
        upload_module.record_upload_latency(i)
    assert len(flags.upload_latency_ms_window) == upload_module.UPLOAD_LATENCY_WINDOW
    # Window should hold the most recent N entries.
    assert flags.upload_latency_ms_window[0] == 200 - upload_module.UPLOAD_LATENCY_WINDOW
    assert flags.upload_latency_ms_window[-1] == 199


def test_p95_helper() -> None:
    """telemetry_poll._p95 — nearest-rank p95 over a small samples list."""
    from ghostcam.telemetry_poll import _p95

    assert _p95([]) == 0
    assert _p95([100]) == 100
    # 100 samples: p95 ≈ 95th value when sorted.
    samples = list(range(1, 101))
    assert _p95(samples) == 95
    # Small list: nearest-rank rounds down.
    assert _p95([1, 2, 3, 4, 5, 6, 7, 8, 9, 10]) == 9


@pytest.mark.asyncio
async def test_pending_announced_in_presign_call(tmp_path: Path) -> None:
    """A batch of successful PUTs surfaces in the NEXT presign call's
    `pending` field. This is the wire signal powering the viewer's
    blue-indicator overlay. The first 3 uploads exhaust the initial
    URL batch; the next presign call (to replenish) carries them as
    `pending`."""
    fake = FakeClient(
        presign_responses=[
            # Initial batch of 3 URLs.
            PresignResponse(urls=[
                _presigned("seg-1"), _presigned("seg-2"), _presigned("seg-3"),
            ]),
            # Replenish call — this is the one that should carry the
            # 3 just-uploaded segments in `pending`.
            PresignResponse(urls=[_presigned("seg-4")]),
            PresignResponse(urls=[]),
        ],
    )
    shared: asyncio.Queue[NewSegment] = asyncio.Queue()
    for i in range(4):
        await shared.put(_make_segment(tmp_path, f"seg0000{i}.ts"))

    task = asyncio.create_task(run_upload_loop(fake, tmp_path, shared))  # type: ignore[arg-type]
    # 4 sequential uploads finish well under 200 ms with the fake client.
    await asyncio.sleep(0.25)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    # Cold start: first presign call has no pending.
    assert fake.presign_calls
    _count0, _uploaded0, pending0 = fake.presign_calls[0]
    assert pending0 == []

    # The replenish call (presign call #2) should carry the first 3
    # segments as pending — they finished uploading but haven't yet
    # confirmed via the `uploaded` path.
    pending_segs = [
        seg
        for _, _uploaded, pending in fake.presign_calls
        for seg in pending
    ]
    announced_ids = {p.segment_id for p in pending_segs}
    assert {"seg-1", "seg-2", "seg-3"} <= announced_ids, \
        f"expected seg-1/2/3 in pending across {fake.presign_calls}"
