"""Telemetry poll loop.

Mirrors camera/telemetry_poll.go. Sends sensor readings to
/api/v1/cameras/{deviceID}/telemetry every 10 s. Backs off to 30 s after
3 consecutive failures, 60 s after 5+. Writes a `boot_ok` marker after
the first success — systemd's ExecStartPre uses this to decide whether
to roll back a staged firmware update.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from pathlib import Path

from ghostcam.client import Client
from ghostcam.platform import read_telemetry
from ghostcam.wire import CameraCommand

logger = logging.getLogger(__name__)

BASE_INTERVAL = 10.0
MAX_INTERVAL = 60.0


async def run_telemetry_poll(
    client: Client,
    data_dir: Path,
    handle_command: Callable[[CameraCommand], Awaitable[None]],
) -> None:
    interval = BASE_INTERVAL
    consecutive_failures = 0
    health_marked = False

    while True:
        await asyncio.sleep(interval)
        telemetry = read_telemetry()
        try:
            commands = await client.post_telemetry(telemetry)
        except Exception as e:  # noqa: BLE001
            consecutive_failures += 1
            logger.debug("telemetry POST failed (%d consecutive): %s",
                         consecutive_failures, e)
            if consecutive_failures >= 3:
                interval = MAX_INTERVAL
            elif consecutive_failures >= 2:
                interval = 30.0
            else:
                interval = BASE_INTERVAL
            continue

        if consecutive_failures > 0:
            consecutive_failures = 0
            interval = BASE_INTERVAL

        if not health_marked:
            try:
                (data_dir / "boot_ok").touch()
                health_marked = True
            except OSError:
                pass

        for cmd in commands:
            try:
                await handle_command(cmd)
            except Exception as e:  # noqa: BLE001
                logger.warning("command handler raised: %s", e)
