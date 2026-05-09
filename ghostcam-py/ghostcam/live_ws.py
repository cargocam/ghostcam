"""Live WebSocket relay to the server.

Mirrors camera/live_ws.go. Maintains a persistent ws:// or wss://
connection to /api/v1/cameras/{deviceID}/live, sends a `{"type":"ready"}`
JSON message after connect, then forwards LiveFrames as binary frames
when a viewer is watching.

Wire format (must match server/live.go):

  [4 bytes BE uint32 timestamp_ms (mod 2^32)]
  [1 byte flags: bit0 = is_keyframe, bit1 = is_audio]
  [payload — H.264 NAL unit OR Opus packet]

When no viewer is connected the server sends `{"type":"stop_stream"}`
and we drop frames silently. `{"type":"start_stream"}` flips streaming
back on. Reconnects with exponential backoff 2 → 30 s on disconnect.
"""

from __future__ import annotations

import asyncio
import json
import logging
import struct
import time

import websockets
from websockets.exceptions import ConnectionClosed

from ghostcam.client import Client
from ghostcam.live_relay import LiveRelay

logger = logging.getLogger(__name__)

INITIAL_BACKOFF = 2.0
MAX_BACKOFF = 30.0


def _build_ws_url(server_url: str, device_id: str) -> str:
    base = server_url.rstrip("/")
    if base.startswith("https://"):
        base = "wss://" + base[len("https://"):]
    elif base.startswith("http://"):
        base = "ws://" + base[len("http://"):]
    return f"{base}/api/v1/cameras/{device_id}/live"


async def run_live_relay(client: Client, relay: LiveRelay) -> None:
    """Persistent reconnect loop.

    Cancellation closes any active connection cleanly via the websockets
    library's context manager.
    """
    backoff = INITIAL_BACKOFF
    while True:
        try:
            await _run_one_session(client, relay)
            backoff = INITIAL_BACKOFF
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            logger.debug("live relay error: %s", e)
        logger.info("live relay reconnecting in %.0fs", backoff)
        await asyncio.sleep(backoff)
        backoff = min(backoff * 2, MAX_BACKOFF)


async def _run_one_session(client: Client, relay: LiveRelay) -> None:
    url = _build_ws_url(client.server_url, client.device_id)
    headers = {"Authorization": client._auth_header("GET", f"/api/v1/cameras/{client.device_id}/live")}
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
        control_task = asyncio.create_task(
            _read_control_messages(conn, streaming),
            name="ws-control",
        )

        try:
            while True:
                frame = await relay.queue.get()
                if not streaming.is_set():
                    continue  # discard frames when no viewer
                payload = _pack_frame(frame.data, frame.is_keyframe, frame.is_audio)
                try:
                    await conn.send(payload)
                except ConnectionClosed:
                    return
        finally:
            control_task.cancel()
            try:
                await control_task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass


async def _read_control_messages(
    conn,  # type: ignore[no-untyped-def]
    streaming: asyncio.Event,
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
    except ConnectionClosed:
        return


def _pack_frame(data: bytes, is_keyframe: bool, is_audio: bool) -> bytes:
    """Wire format from camera/live_ws.go::sendFrame.

    [4 bytes BE uint32 ts_ms (mod 2^32)] [1 byte flags] [payload]
    """
    ts_ms = int(time.time() * 1000) & 0xFFFFFFFF
    flags = 0
    if is_keyframe:
        flags |= 0x01
    if is_audio:
        flags |= 0x02
    return struct.pack(">IB", ts_ms, flags) + data
