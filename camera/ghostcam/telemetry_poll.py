"""Telemetry poll loop.

Sends sensor readings to /api/v1/cameras/{deviceID}/telemetry on a
power-mode-aware cadence:

  * live    — every 10 s (base)
  * standby — every 10 s (so viewer wake-up latency stays bounded)
  * sleep   — every 5 min (configurable)

Backs off to 30 s after 3 consecutive failures, 60 s after 5+ — but
the floor is whatever the current power mode requires, so a sleep-mode
camera doesn't accidentally start polling more frequently when the
server is briefly unreachable.

Writes a `boot_ok` marker after the first success — systemd's
ExecStartPre uses this to decide whether to roll back a staged
firmware update.

Reads `wake_live` off each response. When set (and we're in standby),
we hand off to a `wake_live_callback` so the live WS task can connect
the WebSocket and bring the viewer's session online.

Reports the camera's currently-effective power_mode / upload_mode and
optionally battery_pct back on each telemetry datagram so the UI can
show what schedule / battery rules are doing.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import TYPE_CHECKING

from ghostcam.client import Client
from ghostcam.platform import read_telemetry
from ghostcam.power_mode import PowerModeState
from ghostcam.upload import flags as upload_flags
from ghostcam.wire import CameraCommand

if TYPE_CHECKING:
    from ghostcam.live_relay import LiveRelay
    from ghostcam.watcher import NewSegment

logger = logging.getLogger(__name__)

BASE_INTERVAL = 10.0
MAX_INTERVAL = 60.0


# Type alias for the optional battery reader. Returns 0–100 or None.
BatteryReader = Callable[[], int | None]


async def run_telemetry_poll(
    client: Client,
    data_dir: Path,
    handle_command: Callable[[CameraCommand], Awaitable[None]],
    *,
    power: PowerModeState | None = None,
    wake_live_callback: Callable[[], None] | None = None,
    battery_reader: BatteryReader | None = None,
    live_relay: LiveRelay | None = None,
    segment_queue: asyncio.Queue[NewSegment] | None = None,
) -> None:
    """Long-running telemetry loop.

    `power` is optional so existing tests / partial setups still work,
    but the daemon's main wires it in for power-mode-aware cadence.
    `wake_live_callback` is invoked (sync) when the server sets the
    `wake_live` flag in a poll response — the live WS task hooks here.
    `battery_reader` is the platform/battery hook; today returns None
    until a HAT driver lands (see GH issue #73).
    `live_relay` and `segment_queue` are optional observability hooks
    — when provided, the loop attaches WebSocket throughput and queue-
    depth metrics to each telemetry datagram.
    """
    interval = BASE_INTERVAL
    consecutive_failures = 0
    health_marked = False
    last_tick_at = asyncio.get_event_loop().time()

    while True:
        # Measure how long the sleep over-ran its scheduled deadline.
        # Non-zero here = something blocked the event loop synchronously
        # during the previous interval. Surfaced as
        # TelemetryDatagram.event_loop_lag_ms so it shows up in the UI.
        scheduled_wake = last_tick_at + interval
        await asyncio.sleep(interval)
        woken_at = asyncio.get_event_loop().time()
        event_loop_lag_ms = max(0, int((woken_at - scheduled_wake) * 1000))
        last_tick_at = woken_at
        tick_interval_s = interval

        # Read platform sensors off the event loop so a slow gpsd /
        # nmcli can't stall the live WS or upload tasks.
        telemetry = await asyncio.to_thread(read_telemetry)

        if event_loop_lag_ms:
            telemetry.event_loop_lag_ms = min(event_loop_lag_ms, 65535)

        # Annotate with power-mode and battery state so the server (and UI)
        # can see what the camera is actually doing right now.
        if power is not None:
            eff = power.effective
            telemetry.power_mode = eff.power_mode
            telemetry.upload_mode = eff.upload_mode
        if battery_reader is not None:
            try:
                pct = battery_reader()
            except Exception as e:  # noqa: BLE001
                logger.debug("battery reader raised: %s", e)
                pct = None
            if pct is not None:
                telemetry.battery_pct = pct
                if power is not None:
                    power.set_battery_pct(pct)

        # Motion-gated upload counters — only surface when the camera has
        # actually skipped something, so the absence of the field on
        # constant-mode cameras carries no signal.
        if upload_flags.motion_segments_uploaded or upload_flags.motion_segments_skipped:
            telemetry.motion_segments_uploaded = upload_flags.motion_segments_uploaded
            telemetry.motion_segments_skipped = upload_flags.motion_segments_skipped

        # Upload-path health metrics.
        if upload_flags.upload_retries_total:
            telemetry.segment_upload_retries = upload_flags.upload_retries_total
        if upload_flags.upload_latency_ms_window:
            telemetry.segment_upload_p95_ms = _p95(upload_flags.upload_latency_ms_window)
        if segment_queue is not None:
            telemetry.segment_queue_depth = min(segment_queue.qsize(), 255)

        # Live-WS health metrics.
        if live_relay is not None:
            bytes_in_window = live_relay.pop_byte_tally()
            if bytes_in_window and tick_interval_s > 0:
                telemetry.live_ws_bytes_per_sec = int(bytes_in_window / tick_interval_s)
            if live_relay.dropped_frames_total:
                telemetry.live_ws_dropped_frames = live_relay.dropped_frames_total

        try:
            response = await client.post_telemetry_full(telemetry)
        except Exception as e:  # noqa: BLE001
            consecutive_failures += 1
            logger.debug(
                "telemetry POST failed (%d consecutive): %s",
                consecutive_failures, e,
            )
            interval = _compute_failure_interval(consecutive_failures, power)
            continue

        if consecutive_failures > 0:
            consecutive_failures = 0

        # Refresh interval for the success path. Mode may have changed
        # mid-flight (e.g. the response carried a set_power_mode command,
        # which executes after this re-set — so the NEXT iteration picks
        # up the new cadence).
        interval = power.telemetry_interval_s() if power is not None else BASE_INTERVAL

        if not health_marked:
            try:
                (data_dir / "boot_ok").touch()
                health_marked = True
            except OSError:
                pass

        # Standby-mode wake: hand off to the live WS task.
        if response.wake_live and wake_live_callback is not None:
            try:
                wake_live_callback()
            except Exception as e:  # noqa: BLE001
                logger.warning("wake_live callback raised: %s", e)

        for cmd in response.commands or []:
            try:
                await handle_command(cmd)
            except Exception as e:  # noqa: BLE001
                logger.warning("command handler raised: %s", e)


def _p95(samples: list[int]) -> int:
    """Nearest-rank p95 of a small samples list. Returns 0 on empty."""
    if not samples:
        return 0
    s = sorted(samples)
    idx = max(0, min(len(s) - 1, int(len(s) * 0.95) - 1))
    return s[idx]


def _compute_failure_interval(
    consecutive_failures: int, power: PowerModeState | None,
) -> float:
    """Backoff curve clamped against the current power mode's natural
    interval — never poll FASTER than the mode wants. In sleep mode
    (300 s base) the failure curve flattens because the failure
    intervals are all shorter than the natural one."""
    if consecutive_failures >= 3:
        target = MAX_INTERVAL
    elif consecutive_failures >= 2:
        target = 30.0
    else:
        target = BASE_INTERVAL
    if power is not None:
        return max(target, power.telemetry_interval_s())
    return target
