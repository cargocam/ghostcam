"""Server-issued command dispatch.

Mirrors camera/commands.go originally. Many commands cause the process
to exit so systemd restarts it with new state — same approach as the
Go camera. The new power-mode commands (set_power_mode,
set_upload_mode, set_schedule, set_battery_rules, upload_segments)
take effect WITHOUT a restart by mutating PowerModeState and the
upload queue in-process.
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Callable
from pathlib import Path

from ghostcam.client import Client
from ghostcam.config import write_stored_file
from ghostcam.credentials import clear_credentials
from ghostcam.firmware import check_firmware_update
from ghostcam.platform import ensure_wifi
from ghostcam.power_mode import PowerModeState
from ghostcam.wire import CameraCommand

logger = logging.getLogger(__name__)


# `prioritize_uploads` is the upload loop's hook for `upload_segments`
# commands — given a list of segment IDs, push them to the front of
# the upload queue ahead of normal new-segment work.
PriorityUploadCallback = Callable[[list[str]], None]
# Holder setter / getter for the current recording_mode. When wired up,
# set_recording_mode commands can hot-swap constant ↔ motion without a
# daemon restart.
RecordingModeSetter = Callable[[str], None]
RecordingModeGetter = Callable[[], str]


async def handle_command(
    cmd: CameraCommand,
    data_dir: Path,
    client: Client,
    *,
    power: PowerModeState | None = None,
    prioritize_uploads: PriorityUploadCallback | None = None,
    set_recording_mode: RecordingModeSetter | None = None,
    get_recording_mode: RecordingModeGetter | None = None,
) -> None:
    """Dispatch a single CameraCommand.

    `power` and `prioritize_uploads` are optional so existing call sites
    in tests / partial setups still work, but the daemon's main wires
    them in for the new commands to actually take effect.
    """
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
        new_mode = cmd.mode
        logger.info("recording mode change requested: %s", new_mode)
        try:
            write_stored_file(data_dir, "recording_mode", new_mode)
        except OSError as e:
            logger.error("failed to persist recording_mode: %s", e)
            return
        # Hot-swap when only the upload-decision flag changes (constant
        # ↔ motion). The upload loop reads via get_recording_mode each
        # iteration. Transitions to/from "never" still require a restart
        # because the watcher and upload tasks are conditionally spawned
        # in main.py based on the boot-time mode.
        current_mode = get_recording_mode() if get_recording_mode is not None else None
        if (
            set_recording_mode is not None
            and current_mode is not None
            and current_mode != "never"
            and new_mode != "never"
        ):
            set_recording_mode(new_mode)
            logger.info("recording mode hot-swapped: %s → %s", current_mode, new_mode)
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

    # --- power-mode commands ---
    elif cmd.type == "set_power_mode":
        if not cmd.power_mode or power is None:
            return
        logger.info("power_mode change requested: %s", cmd.power_mode)
        try:
            power.set_manual_power_mode(cmd.power_mode)
        except ValueError as e:
            logger.warning("ignoring invalid power_mode: %s", e)
    elif cmd.type == "set_upload_mode":
        if not cmd.upload_mode or power is None:
            return
        logger.info("upload_mode change requested: %s", cmd.upload_mode)
        try:
            power.set_manual_upload_mode(cmd.upload_mode)
        except ValueError as e:
            logger.warning("ignoring invalid upload_mode: %s", e)
    elif cmd.type == "set_schedule":
        if power is None:
            return
        # Empty string is "clear schedule".
        body = cmd.schedule or "[]"
        logger.info("schedule update received (%d chars)", len(body))
        try:
            power.set_schedule(body)
        except (ValueError, KeyError, TypeError) as e:
            logger.warning("ignoring malformed schedule: %s", e)
    elif cmd.type == "set_battery_rules":
        if power is None:
            return
        body = cmd.battery_rules or "[]"
        logger.info("battery rules update received (%d chars)", len(body))
        try:
            power.set_battery_rules(body)
        except (ValueError, KeyError, TypeError) as e:
            logger.warning("ignoring malformed battery rules: %s", e)
    elif cmd.type == "upload_segments":
        if not cmd.segment_ids:
            return
        logger.info(
            "upload_segments command: %d segment(s) requested",
            len(cmd.segment_ids),
        )
        if prioritize_uploads is None:
            logger.warning(
                "upload_segments received but no upload-prioritise hook wired in"
            )
            return
        prioritize_uploads(list(cmd.segment_ids))

    else:
        logger.warning("unknown command type: %s", cmd.type)


async def _safe_ensure_wifi(ssid: str, psk: str | None) -> None:
    try:
        await ensure_wifi(ssid, psk)
    except Exception as e:  # noqa: BLE001
        logger.warning("WiFi config failed: %s", e)
