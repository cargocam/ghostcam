"""First-boot provisioning.

Mirrors camera/provisioning.go. Resolution order for the provision token
and server URL is CLI/env → flat files in data_dir → QR code scan.

QR scanning is platform-gated (real-Pi only via rpicam-still + pyzbar);
on synthetic platforms `scan_qr` returns None, so unprovisioned cameras
in dev/Docker just need the env vars.
"""

from __future__ import annotations

import contextlib
import logging
from pathlib import Path

from ghostcam.client import provision
from ghostcam.config import CameraConfig
from ghostcam.credentials import Credentials, save_credentials
from ghostcam.identity import Identity
from ghostcam.platform import ensure_wifi, scan_qr, wait_for_route

logger = logging.getLogger(__name__)


def _read_trimmed(path: Path) -> str:
    try:
        return path.read_text().strip()
    except OSError:
        return ""


def _resolve_inputs(cfg: CameraConfig) -> tuple[str, str]:
    token = cfg.provision_token
    server_url = cfg.server_url
    if not token:
        token = _read_trimmed(cfg.data_dir / "provision_token")
    if not server_url:
        server_url = _read_trimmed(cfg.data_dir / "server_url")
    if token and server_url:
        return token, server_url
    return "", ""


async def run_provisioning(
    cfg: CameraConfig,
    device_serial: str,
    identity: Identity,
) -> Credentials | None:
    token, server_url = _resolve_inputs(cfg)

    if not token or not server_url:
        qr = await scan_qr()
        if qr is None:
            logger.info("no provision token available, waiting for provisioning")
            return None
        token = qr.t
        server_url = qr.s
        if qr.w:
            try:
                await ensure_wifi(qr.w, qr.p)
            except Exception as e:  # noqa: BLE001
                logger.warning("WiFi from QR failed (ssid=%s): %s", qr.w, e)
            await wait_for_route()

    if not server_url.startswith(("http://", "https://")):
        server_url = "https://" + server_url

    logger.info("attempting provisioning: server=%s", server_url)
    await provision(server_url, token, device_serial, identity)

    creds = Credentials(
        device_id=identity.device_id,
        server_url=server_url,
        identity=identity,
    )
    save_credentials(cfg.data_dir, server_url)

    token_file = cfg.data_dir / "provision_token"
    if token_file.exists():
        with contextlib.suppress(OSError):
            token_file.unlink()

    logger.info("provisioning complete: device_id=%s", creds.device_id)
    return creds
