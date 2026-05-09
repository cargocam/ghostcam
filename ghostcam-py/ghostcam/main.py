"""Camera entry point.

Mirrors camera/main.go. Five concurrent goroutines map to five asyncio
tasks under one TaskGroup:

  * live_ws.run_live_relay      — persistent WebSocket to the server
  * capture.start_capture_pipeline — rpicam-vid + ffmpeg orchestration
  * watcher.run_segment_watcher + upload.run_upload_loop (when recording)
  * telemetry_poll.run_telemetry_poll

Provisioning runs as a one-shot before the TaskGroup. Graceful shutdown:
SIGTERM/SIGINT cancels the group with a 15 s drain budget, then exits.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import signal
import sys
from functools import partial

from ghostcam.client import Client
from ghostcam.commands import handle_command
from ghostcam.config import load_config
from ghostcam.credentials import load_credentials
from ghostcam.firmware import check_firmware_update
from ghostcam.identity import load_or_create_identity
from ghostcam.live_relay import LiveRelay
from ghostcam.live_ws import run_live_relay
from ghostcam.platform import (
    get_device_serial,
    set_gps_seed,
    wait_for_route,
)
from ghostcam.provisioning import run_provisioning
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
    try:
        if await check_firmware_update(client, cfg.data_dir):
            return 0  # systemd restarts; ExecStartPre installs

        relay = LiveRelay()
        segments: asyncio.Queue[NewSegment] = asyncio.Queue(maxsize=256)
        cmd_handler = partial(handle_command, data_dir=cfg.data_dir, client=client)

        async def _supervise_capture() -> None:
            from ghostcam.capture import start_capture_pipeline
            from ghostcam.upload import flags

            backoff = 1.0
            stable_after = 300.0  # 5 minutes
            crash_count = 0

            while True:
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

        async with asyncio.TaskGroup() as tg:
            tg.create_task(run_live_relay(client, relay), name="live-relay")
            tg.create_task(_supervise_capture(), name="capture-supervisor")
            tg.create_task(
                run_telemetry_poll(client, cfg.data_dir, cmd_handler),
                name="telemetry-poll",
            )
            if cfg.recording_mode != "never":
                tg.create_task(
                    run_segment_watcher(
                        cfg.segment_dir,
                        cfg.data_dir,
                        cfg.local_storage_cap_bytes,
                        segments,
                    ),
                    name="segment-watcher",
                )
                tg.create_task(
                    run_upload_loop(client, cfg.data_dir, segments),
                    name="upload-loop",
                )
            else:
                logger.info("recording_mode=never — skipping watcher and upload")

    except asyncio.CancelledError:
        pass
    finally:
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
