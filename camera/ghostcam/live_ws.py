"""Live WebSocket relay to the server.

Maintains a ws:// or wss:// connection to
/api/v1/cameras/{deviceID}/live, sends a `{"type":"ready"}` JSON
message after connect, then forwards LiveFrames as binary frames when
a viewer is watching.

Wire format (must match server/live.go):

  [4 bytes BE uint32 timestamp_ms (mod 2^32)]
  [1 byte flags: bit0 = is_keyframe, bit1 = is_audio]
  [payload — H.264 NAL unit OR Opus packet]

Power-mode behaviour:

  * `live` mode: WS is held open continuously; auto-reconnects with
    exponential backoff 2 → 30 s on disconnect. This is the original
    behaviour.

  * `standby` mode: WS sleeps until a `wake_live` flag arrives via
    telemetry-poll. The driver task wakes via `wake()`, runs one
    session, and goes back to sleep when the server sends
    `stop_stream` and stays quiet for `IDLE_AUTOCLOSE_S` seconds.

  * `sleep` mode: WS task is not started at all (capture is off too,
    so there are no frames to relay).

The `LiveWSDriver` class encapsulates the wake/sleep state machine
across reconnect cycles. `run_live_relay` is kept as the legacy entry
point for the always-on (live-mode) case so existing tests don't need
to change.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import struct
import time
from typing import Any

import websockets
from websockets.exceptions import ConnectionClosed

from ghostcam.client import Client
from ghostcam.live_relay import LiveRelay
from ghostcam.power_mode import PowerModeState

logger = logging.getLogger(__name__)

INITIAL_BACKOFF = 2.0
MAX_BACKOFF = 30.0
# In standby mode, after the server sends stop_stream we wait this long
# for a fresh start_stream before tearing the WS down. Keeps the camera
# responsive within a single viewer's session without holding the radio
# in CONNECTED for the gap between viewers.
IDLE_AUTOCLOSE_S = 30.0


def _build_ws_url(server_url: str, device_id: str) -> str:
    base = server_url.rstrip("/")
    if base.startswith("https://"):
        base = "wss://" + base[len("https://"):]
    elif base.startswith("http://"):
        base = "ws://" + base[len("http://"):]
    return f"{base}/api/v1/cameras/{device_id}/live"


# --- legacy always-on entry point ----------------------------------------


async def run_live_relay(client: Client, relay: LiveRelay) -> None:
    """Persistent reconnect loop for `live` power mode (the default).

    Wraps `_run_one_session` with exponential backoff. Cancellation
    closes any active connection cleanly via the websockets library's
    context manager.
    """
    backoff = INITIAL_BACKOFF
    while True:
        try:
            await _run_one_session(client, relay, idle_autoclose_s=None)
            backoff = INITIAL_BACKOFF
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            logger.debug("live relay error: %s", e)
        logger.info("live relay reconnecting in %.0fs", backoff)
        await asyncio.sleep(backoff)
        backoff = min(backoff * 2, MAX_BACKOFF)


# --- standby driver: wake on demand --------------------------------------


class LiveWSDriver:
    """Power-mode-aware WS driver.

    In `live` mode it loops forever like `run_live_relay`. In `standby`
    mode it sleeps on a `_wake_event` until `wake()` is called, runs
    one session with idle-autoclose semantics, and goes back to sleep.
    Mode changes are observed via `power.changed`.
    """

    def __init__(self, client: Client, relay: LiveRelay, power: PowerModeState) -> None:
        self._client = client
        self._relay = relay
        self._power = power
        self._wake_event = asyncio.Event()

    def wake(self) -> None:
        """Hand-off from telemetry_poll: server set wake_live=true."""
        self._wake_event.set()

    async def run(self) -> None:
        backoff = INITIAL_BACKOFF
        while True:
            mode = self._power.power_mode
            if mode == "sleep":
                # Capture is off too; just wait for a mode change.
                logger.debug("live WS driver: sleep mode, idling")
                await self._wait_for_mode_change()
                continue

            if mode == "standby":
                # Wait either for an explicit wake_live or for a return
                # to live mode.
                self._wake_event.clear()
                logger.debug("live WS driver: standby, awaiting wake_live")
                await asyncio.wait(
                    [
                        asyncio.create_task(self._wake_event.wait()),
                        asyncio.create_task(self._wait_for_mode_change()),
                    ],
                    return_when=asyncio.FIRST_COMPLETED,
                )
                if self._power.power_mode != "standby":
                    continue  # mode changed; re-evaluate at top of loop
                idle_autoclose: float | None = IDLE_AUTOCLOSE_S
            else:  # live
                idle_autoclose = None

            # Run one session.
            try:
                await _run_one_session(
                    self._client, self._relay, idle_autoclose_s=idle_autoclose,
                )
                backoff = INITIAL_BACKOFF
            except asyncio.CancelledError:
                raise
            except Exception as e:  # noqa: BLE001
                logger.debug("live relay error: %s", e)
                if self._power.power_mode == "live":
                    # Live mode reconnects with backoff. Standby goes
                    # back to sleep; the next wake_live will retry.
                    logger.info("live relay reconnecting in %.0fs", backoff)
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2, MAX_BACKOFF)

    async def _wait_for_mode_change(self) -> None:
        """Subscribe to power_mode transitions. The Event auto-clears
        immediately after firing (see PowerModeState.recompute), so we
        just await it and bail."""
        await self._power.changed.wait()


# --- shared session loop -------------------------------------------------


async def _run_one_session(
    client: Client, relay: LiveRelay, *, idle_autoclose_s: float | None,
) -> None:
    """One WS connection lifecycle.

    `idle_autoclose_s` controls standby-mode tear-down: when set, after
    the server sends `stop_stream` we wait this many seconds for a
    fresh `start_stream` before closing the WS. None disables the
    auto-close (live-mode behaviour).
    """
    url = _build_ws_url(client.server_url, client.device_id)
    auth_header = client._auth_header(
        "GET", f"/api/v1/cameras/{client.device_id}/live",
    )
    headers = {"Authorization": auth_header}
    logger.info("live relay connecting: %s", url)

    async with websockets.connect(
        url,
        additional_headers=headers,
        ping_interval=20,
        ping_timeout=20,
        max_size=None,  # binary frames may be large NAL units
    ) as conn:
        await conn.send(json.dumps({"type": "ready"}))
        logger.info("live relay connected")

        streaming = asyncio.Event()
        last_stop = asyncio.get_event_loop().time()
        control_task = asyncio.create_task(
            _read_control_messages(conn, streaming, on_stop=lambda: None),
            name="ws-control",
        )

        try:
            while True:
                # Standby self-close: if we've been idle past the
                # autoclose window with no viewer, drop the WS.
                if idle_autoclose_s is not None and not streaming.is_set():
                    elapsed = asyncio.get_event_loop().time() - last_stop
                    remaining = idle_autoclose_s - elapsed
                    if remaining <= 0:
                        logger.info(
                            "standby live WS idle for %.0fs, closing",
                            idle_autoclose_s,
                        )
                        return
                    # Race the queue against the autoclose timer.
                    try:
                        frame = await asyncio.wait_for(
                            relay.queue.get(), timeout=remaining,
                        )
                    except TimeoutError:
                        continue
                else:
                    frame = await relay.queue.get()

                if streaming.is_set():
                    payload = _pack_frame(
                        frame.data, frame.is_keyframe, frame.is_audio,
                    )
                    try:
                        await conn.send(payload)
                    except ConnectionClosed:
                        return
                else:
                    # Reset the stop timer when transitioning into
                    # not-streaming so we measure idleness from the
                    # most recent stop_stream.
                    last_stop = asyncio.get_event_loop().time()
        finally:
            control_task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await control_task


async def _read_control_messages(
    conn: Any,
    streaming: asyncio.Event,
    *,
    on_stop: Any = None,
) -> None:
    try:
        async for msg in conn:
            if isinstance(msg, bytes):
                continue
            try:
                obj = json.loads(msg)
            except json.JSONDecodeError:
                continue
            kind = obj.get("type")
            if kind == "start_stream":
                logger.info("live relay: viewer connected, starting stream")
                streaming.set()
            elif kind == "stop_stream":
                logger.info("live relay: no viewers, stopping stream")
                streaming.clear()
                if on_stop is not None:
                    try:
                        on_stop()
                    except Exception as e:  # noqa: BLE001
                        logger.debug("on_stop callback raised: %s", e)
    except ConnectionClosed:
        return


def _pack_frame(data: bytes, is_keyframe: bool, is_audio: bool) -> bytes:
    """Wire format: [4 bytes BE uint32 ts_ms] [1 byte flags] [payload]."""
    ts_ms = int(time.time() * 1000) & 0xFFFFFFFF
    flags = 0
    if is_keyframe:
        flags |= 0x01
    if is_audio:
        flags |= 0x02
    return struct.pack(">IB", ts_ms, flags) + data
