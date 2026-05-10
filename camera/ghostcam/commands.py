"""Server-issued command dispatch.

Mirrors camera/commands.go. Many commands cause the process to exit so
systemd restarts it with new state — same approach as the Go camera.
"""

from __future__ import annotations

import asyncio
import logging
import os
from pathlib import Path

from ghostcam.client import Client
from ghostcam.config import write_stored_file
from ghostcam.credentials import clear_credentials
from ghostcam.firmware import check_firmware_update
from ghostcam.platform import ensure_wifi
from ghostcam.wire import CameraCommand

logger = logging.getLogger(__name__)


async def handle_command(cmd: CameraCommand, data_dir: Path, client: Client) -> None:
    if cmd.type == "reboot":
        logger.info("reboot command received")
        os._exit(0)
    elif cmd.type == "unregister":
        logger.info("unregister command received, clearing credentials")
        clear_credentials(data_dir)
        os._exit(0)
    elif cmd.type == "set_recording_mode":
        if cmd.mode is None:
            return
        logger.info("recording mode change requested: %s", cmd.mode)
        try:
            write_stored_file(data_dir, "recording_mode", cmd.mode)
        except OSError as e:
            logger.error("failed to persist recording_mode: %s", e)
            return
        logger.info("recording mode updated, restarting to apply")
        os._exit(0)
    elif cmd.type == "set_resolution":
        if cmd.resolution is None:
            return
        logger.info("resolution change requested: %s", cmd.resolution)
        try:
            write_stored_file(data_dir, "resolution", cmd.resolution)
        except OSError as e:
            logger.error("failed to persist resolution: %s", e)
            return
        logger.info("resolution updated, restarting to apply")
        os._exit(0)
    elif cmd.type == "network_config":
        logger.info("network config command: ssid=%s", cmd.ssid)
        if cmd.ssid:
            asyncio.create_task(_safe_ensure_wifi(cmd.ssid, cmd.psk))
    elif cmd.type == "update_firmware":
        logger.info("firmware update command received")
        if await check_firmware_update(client, data_dir):
            os._exit(0)
    elif cmd.type == "remove_network":
        logger.info("remove network command: ssid=%s", cmd.ssid)
    else:
        logger.warning("unknown command type: %s", cmd.type)


async def _safe_ensure_wifi(ssid: str, psk: str | None) -> None:
    try:
        await ensure_wifi(ssid, psk)
    except Exception as e:  # noqa: BLE001
        logger.warning("WiFi config failed: %s", e)
