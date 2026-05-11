"""Camera entry point.

Five concurrent goroutines (originally) → asyncio tasks under one
TaskGroup:

  * live_ws         — persistent WebSocket to the server
  * capture         — rpicam-vid + ffmpeg orchestration
  * watcher         — picks up finished .ts segments
  * upload          — drains segments to S3
  * telemetry_poll  — sensor readings + command queue

Power-mode integration (this layer):

  * `PowerModeState` is loaded once at boot and passed to every task
    that cares (telemetry_poll for cadence + battery; live_ws via
    `LiveWSDriver` for wake-on-demand; upload for lazy gating).
  * `SegmentIndex` is the SQLite-backed manifest the watcher writes
    to and the upload loop reads from. Replaces the legacy
    `pending_confirms.json` for new deployments.
  * Sleep-mode (capture off): the supervisor doesn't spawn the
    pipeline; only telemetry_poll is active so commands and
    schedule re-evaluations still flow.

Provisioning runs as a one-shot before the TaskGroup. Graceful
shutdown: SIGTERM/SIGINT cancels the group with a 15 s drain budget.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import signal
import sys
from collections import deque
from functools import partial

from ghostcam.client import Client
from ghostcam.commands import handle_command
from ghostcam.config import load_config
from ghostcam.credentials import load_credentials
from ghostcam.firmware import check_firmware_update
from ghostcam.identity import load_or_create_identity
from ghostcam.live_relay import LiveRelay
from ghostcam.live_ws import LiveWSDriver
from ghostcam.platform import (
    get_device_serial,
    set_gps_seed,
    wait_for_route,
)
from ghostcam.power_mode import load as load_power_mode
from ghostcam.provisioning import run_provisioning
from ghostcam.segment_index import SegmentIndex
from ghostcam.telemetry_poll import run_telemetry_poll
from ghostcam.upload import run_upload_loop
from ghostcam.watcher import NewSegment, run_segment_watcher

SHUTDOWN_TIMEOUT = 15.0

logger = logging.getLogger(__name__)


async def _amain() -> int:
    cfg = load_config()

    cfg.data_dir.mkdir(parents=True, exist_ok=True)
    cfg.segment_dir.mkdir(parents=True, exist_ok=True)

    device_serial = get_device_serial(cfg.data_dir)
    logger.info("device identity: serial=%s", device_serial)
    set_gps_seed(device_serial)

    identity = load_or_create_identity(cfg.data_dir)
    logger.info("camera identity: device_id=%s", identity.device_id)

    creds = load_credentials(cfg.data_dir)

    if creds is not None and cfg.server_url and cfg.server_url != creds.server_url:
        logger.info("server URL changed (%s -> %s), re-provisioning",
                    creds.server_url, cfg.server_url)
        creds = None

    if creds is not None:
        await wait_for_route()
    else:
        ok = await wait_for_route(timeout=10.0)
        if not ok:
            logger.info("no network after 10s, proceeding to provisioning (QR may provide WiFi)")

        logger.info("no credentials, entering provisioning")
        try:
            creds = await run_provisioning(cfg, device_serial, identity)
        except Exception as e:  # noqa: BLE001
            logger.error("provisioning failed: %s", e)
            return 1
        if creds is None:
            logger.error("no provision_token available and no credentials found")
            return 1
        await wait_for_route()

    if not cfg.server_url:
        cfg.server_url = creds.server_url

    logger.info(
        "starting camera: device_id=%s server=%s test_source=%s",
        creds.device_id, cfg.server_url, cfg.test_source,
    )

    client = Client(server_url=cfg.server_url, device_id=creds.device_id, identity=identity)
    index = SegmentIndex(cfg.data_dir)
    power = load_power_mode(cfg.data_dir)
    logger.info(
        "power_mode at boot: %s/%s (%s)",
        power.power_mode, power.upload_mode, power.effective.source,
    )
    # Server-pushed `upload_segments` commands push IDs onto this deque;
    # the upload loop drains it before the regular new-segment queue.
    priority_uploads: deque[str] = deque()

    try:
        if await check_firmware_update(client, cfg.data_dir):
            return 0  # systemd restarts; ExecStartPre installs

        relay = LiveRelay()
        segments: asyncio.Queue[NewSegment] = asyncio.Queue(maxsize=256)
        live_driver = LiveWSDriver(client, relay, power)

        cmd_handler = partial(
            handle_command,
            data_dir=cfg.data_dir,
            client=client,
            power=power,
            prioritize_uploads=lambda ids: priority_uploads.extend(ids),
        )

        async def _supervise_capture() -> None:
            from ghostcam.capture import start_capture_pipeline
            from ghostcam.upload import flags

            backoff = 1.0
            stable_after = 300.0  # 5 minutes
            crash_count = 0

            while True:
                # Sleep mode: capture off entirely. Wait for a mode
                # change before considering re-launching the pipeline.
                if not power.should_capture():
                    logger.info("capture suppressed: power_mode=%s", power.power_mode)
                    await power.changed.wait()
                    continue

                while flags.server_unreachable:
                    logger.debug("capture paused, server unreachable")
                    await asyncio.sleep(10.0)

                start = asyncio.get_running_loop().time()
                try:
                    await start_capture_pipeline(cfg, relay)
                except asyncio.CancelledError:
                    raise
                except Exception as e:  # noqa: BLE001
                    logger.error("capture pipeline failed: %s", e)
                else:
                    logger.info("capture pipeline exited cleanly")

                elapsed = asyncio.get_running_loop().time() - start
                if elapsed > stable_after:
                    backoff = 1.0
                    crash_count = 0
                else:
                    crash_count += 1
                    if crash_count >= 5:
                        logger.error("capture pipeline unstable: %d crashes", crash_count)

                logger.info("restarting capture pipeline in %.1fs (crashes=%d)",
                            backoff, crash_count)
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

        async def _schedule_ticker() -> None:
            """Re-evaluate schedule + battery rules every minute so
            time-of-day transitions take effect without waiting for the
            next telemetry poll."""
            while True:
                await asyncio.sleep(60.0)
                power.recompute()

        async with asyncio.TaskGroup() as tg:
            tg.create_task(live_driver.run(), name="live-ws")
            tg.create_task(_supervise_capture(), name="capture-supervisor")
            tg.create_task(_schedule_ticker(), name="schedule-ticker")
            tg.create_task(
                run_telemetry_poll(
                    client,
                    cfg.data_dir,
                    cmd_handler,
                    power=power,
                    wake_live_callback=live_driver.wake,
                    battery_reader=None,  # see GH #73 — HAT driver landing later
                ),
                name="telemetry-poll",
            )
            if cfg.recording_mode != "never":
                tg.create_task(
                    run_segment_watcher(
                        cfg.segment_dir,
                        cfg.data_dir,
                        cfg.local_storage_cap_bytes,
                        segments,
                        index=index,
                    ),
                    name="segment-watcher",
                )
                tg.create_task(
                    run_upload_loop(
                        client, cfg.data_dir, segments,
                        index=index,
                        power=power,
                        priority=priority_uploads,
                        recording_mode=cfg.recording_mode,
                    ),
                    name="upload-loop",
                )
            else:
                logger.info("recording_mode=never — skipping watcher and upload")

    except asyncio.CancelledError:
        pass
    finally:
        index.close()
        await client.aclose()

    logger.info("goodbye")
    return 0


def _install_signal_handlers(loop: asyncio.AbstractEventLoop, root_task: asyncio.Task[int]) -> None:
    def _cancel(signame: str) -> None:
        logger.info("received %s, shutting down (15s drain)", signame)
        root_task.cancel()

    for sig in (signal.SIGINT, signal.SIGTERM):
        # Windows / non-main-thread loops don't support add_signal_handler.
        with contextlib.suppress(NotImplementedError, RuntimeError):
            loop.add_signal_handler(sig, _cancel, sig.name)


def run() -> None:
    """Synchronous console entry point. Wired in pyproject.toml."""
    logging.basicConfig(
        level=os.environ.get("GHOSTCAM_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    rc = asyncio.run(_run_with_shutdown())
    sys.exit(rc)


async def _run_with_shutdown() -> int:
    loop = asyncio.get_running_loop()
    task: asyncio.Task[int] = asyncio.create_task(_amain(), name="ghostcam-main")
    _install_signal_handlers(loop, task)
    try:
        return await task
    except SystemExit as e:
        # argparse / sys.exit() inside the task — propagate the code.
        return int(e.code or 0)
    except asyncio.CancelledError:
        try:
            await asyncio.wait_for(asyncio.shield(task), timeout=SHUTDOWN_TIMEOUT)
        except (TimeoutError, asyncio.CancelledError):
            logger.warning("shutdown timeout, some tasks did not drain")
        except SystemExit as e:
            return int(e.code or 0)
        return 0


if __name__ == "__main__":
    run()
