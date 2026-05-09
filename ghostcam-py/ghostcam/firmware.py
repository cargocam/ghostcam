"""Firmware self-update.

Mirrors camera/firmware.go. Polls /api/v1/firmware/latest, verifies the
sha256 of the downloaded artifact, stages it as `staged-update.deb` in
the data dir, and signals the caller (which exits) so systemd's
ExecStartPre can install it.

`VERSION` is set at build time via the wheel/deb. The sentinel "dev"
disables the firmware check entirely — same convention as the Go
camera's `var Version = "dev"`.
"""

from __future__ import annotations

import hashlib
import logging
from pathlib import Path

import httpx
from pydantic import BaseModel, Field

from ghostcam.client import VERSION, Client

logger = logging.getLogger(__name__)


class FirmwareRelease(BaseModel):
    version: str
    download_url: str
    sha256: str = ""


class FirmwareResponse(BaseModel):
    release: FirmwareRelease | None = Field(default=None)


async def check_firmware_update(client: Client, data_dir: Path) -> bool:
    """Returns True if a new firmware was staged. The caller should exit
    so systemd restarts the service."""
    if VERSION == "dev":
        logger.debug("firmware check skipped (dev build)")
        return False

    logger.info("checking for firmware update (current=%s)", VERSION)

    try:
        resp = await _get_firmware_latest(client)
    except Exception as e:  # noqa: BLE001
        logger.warning("firmware check failed: %s", e)
        return False

    if resp.release is None:
        logger.debug("no firmware release available")
        return False

    if resp.release.version == VERSION:
        logger.debug("firmware up to date (%s)", VERSION)
        return False

    logger.info(
        "new firmware available: %s -> %s",
        VERSION, resp.release.version,
    )

    staged = data_dir / "staged-update.deb"
    try:
        await _download_to_file(resp.release.download_url, staged)
    except Exception as e:  # noqa: BLE001
        logger.error("firmware download failed: %s", e)
        return False

    if resp.release.sha256:
        actual = _sha256_file(staged)
        if actual != resp.release.sha256:
            logger.error("firmware hash mismatch (expected=%s actual=%s)",
                         resp.release.sha256, actual)
            try:
                staged.unlink()
            except OSError:
                pass
            return False
        logger.info("firmware hash verified: %s", actual)

    logger.info("firmware staged, restart will apply: %s", staged)
    return True


async def _get_firmware_latest(client: Client) -> FirmwareResponse:
    url = client.server_url + "/api/v1/firmware/latest"
    http = await client._client()
    resp = await http.get(url, timeout=10.0)
    if resp.status_code != 200:
        raise RuntimeError(f"firmware/latest returned {resp.status_code}")
    return FirmwareResponse.model_validate(resp.json())


async def _download_to_file(url: str, dest: Path) -> None:
    tmp = dest.with_suffix(dest.suffix + ".tmp")
    async with httpx.AsyncClient(timeout=300.0) as http:
        async with http.stream("GET", url) as resp:
            if resp.status_code != 200:
                raise RuntimeError(f"download returned {resp.status_code}")
            with tmp.open("wb") as f:
                async for chunk in resp.aiter_bytes(65536):
                    f.write(chunk)
    tmp.replace(dest)


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        while chunk := f.read(65536):
            h.update(chunk)
    return h.hexdigest()
