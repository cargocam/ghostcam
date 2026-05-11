"""POST /api/v1/cameras/provision: end-to-end against the real server.

Asserts that:
  * The Python identity module's device_id derivation matches the
    server's (server stores it; we read it back).
  * The provision response carries the expected device_id and status.
  * After provisioning, the camera can issue an authenticated request
    (ed25519-signed POST /telemetry) and the server accepts it.
"""

from __future__ import annotations

from pathlib import Path

import httpx
import pytest

from ghostcam.client import Client, provision
from ghostcam.identity import load_or_create_identity
from ghostcam.platform import set_gps_seed
from ghostcam.wire import TelemetryDatagram


@pytest.mark.asyncio
async def test_provision_then_telemetry_round_trip(
    server: dict[str, str],
    provision_token: str,
    tmp_path: Path,
) -> None:
    set_gps_seed("integration-test-camera")
    identity = load_or_create_identity(tmp_path)

    resp = await provision(
        server_url=server["url"],
        token=provision_token,
        device_serial="integration-serial",
        identity=identity,
    )
    assert resp.device_id == identity.device_id, (
        "server-derived device_id must match the camera's local derivation "
        f"(server={resp.device_id} local={identity.device_id}); a mismatch "
        "means SHA-256(public_key)[:16] hex differs across implementations."
    )
    assert resp.status == "registered"

    # Now post telemetry — exercises ed25519 signing through to the
    # server's auth/verify.go on a real socket.
    client = Client(
        server_url=server["url"],
        device_id=identity.device_id,
        identity=identity,
    )
    try:
        commands = await client.post_telemetry(TelemetryDatagram(
            ts=1,
            cpu=10,
            mem=128,
            temp=42,
            uptime=60,
        ))
        # Fresh enrollment: no commands queued.
        assert isinstance(commands, list)
    finally:
        await client.aclose()


@pytest.mark.asyncio
async def test_unsigned_request_rejected(server: dict[str, str]) -> None:
    """A request without the Signature header must 401, otherwise the
    server has a critical auth bypass."""
    async with httpx.AsyncClient(base_url=server["url"], timeout=5.0) as http:
        r = await http.post(
            "/api/v1/cameras/0123456789abcdef/telemetry",
            json={"telemetry": {"ts": 1}},
        )
    assert r.status_code in (401, 403)
