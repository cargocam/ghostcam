"""Platform abstraction.

Replaces the Go camera's build tags. The real-Pi implementation lives in
`linux.py` (gated on the existence of /proc); the synthetic stubs live
in `synthetic.py`. Selection happens at import time:

  * `GHOSTCAM_SYNTHETIC=1` → always synthetic (Docker, CI, tests).
  * Otherwise on Linux with /proc → real sensors.
  * Otherwise → synthetic (macOS, Windows dev).

The chosen module is re-exported here, so callers say
`from ghostcam.platform import read_telemetry, get_device_serial, ...`.
"""

from __future__ import annotations

import logging
import os
import platform
import secrets
import time
import uuid
from pathlib import Path
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from ghostcam.wire import QRPayload, TelemetryDatagram

logger = logging.getLogger(__name__)


# Initialized at import time by load_platform(); the chosen module is
# referenced through these names.

_USE_SYNTHETIC: bool


def _select() -> bool:
    if os.environ.get("GHOSTCAM_SYNTHETIC", "") == "1":
        return True
    if platform.system() != "Linux":
        return True
    return not Path("/proc/cpuinfo").exists()


_USE_SYNTHETIC = _select()

if _USE_SYNTHETIC:
    from ghostcam.platform import synthetic as _impl  # noqa: F401
    logger.debug("platform: synthetic sensors")
else:
    from ghostcam.platform import linux as _impl  # type: ignore[no-redef]  # noqa: F811
    logger.debug("platform: real Linux sensors")


# --- common helpers (always available, no build-tag equivalent in Go) ---


def now_ms() -> int:
    """Unix milliseconds. Mirrors camera/sensors_common.go::nowMillis."""
    return int(time.time() * 1000)


_GPS_SEED = ""


def set_gps_seed(seed: str) -> None:
    """Seed the synthetic GPS orbit so multiple cameras cluster
    deterministically. The real implementation ignores this."""
    global _GPS_SEED
    _GPS_SEED = seed
    if hasattr(_impl, "set_gps_seed"):
        _impl.set_gps_seed(seed)


def get_gps_seed() -> str:
    return _GPS_SEED


def gen_uuid() -> str:
    """Generate a random v4 UUID. Mirrors camera/sensors_common.go::generateUUID."""
    return str(uuid.UUID(bytes=secrets.token_bytes(16), version=4))


def _generate_and_store_serial(data_dir: Path) -> str:
    serial = gen_uuid()
    data_dir.mkdir(parents=True, exist_ok=True)
    (data_dir / "device_serial").write_text(serial)
    return serial


# --- re-exports from selected platform impl ---


def get_device_serial(data_dir: Path) -> str:
    """Return the persisted device serial, falling back to /proc/cpuinfo
    on real Linux or generating a UUID on synthetic.

    Mirrors camera/sensors_{linux,other}.go::GetDeviceSerial.
    """
    stored = data_dir / "device_serial"
    if stored.exists():
        s = stored.read_text().strip()
        if s:
            return s

    serial = _impl.read_device_serial(data_dir)
    if serial:
        data_dir.mkdir(parents=True, exist_ok=True)
        stored.write_text(serial)
        return serial
    return _generate_and_store_serial(data_dir)


def read_telemetry() -> "TelemetryDatagram":  # noqa: UP037
    """Return a fully-populated TelemetryDatagram for the current platform."""
    return _impl.read_telemetry()


async def wait_for_route(timeout: float | None = None) -> bool:
    """Block until a default route exists (or timeout). Returns True on
    success. Mirrors camera/network_*.go.
    """
    if hasattr(_impl, "wait_for_route"):
        return bool(await _impl.wait_for_route(timeout))
    return True


async def ensure_wifi(ssid: str, psk: str | None) -> None:
    """Connect to the given WiFi network (no-op on synthetic)."""
    if hasattr(_impl, "ensure_wifi"):
        await _impl.ensure_wifi(ssid, psk)


async def scan_qr(timeout: float = 300.0) -> "QRPayload | None":  # noqa: UP037
    """Scan the camera sensor for a provisioning QR. Returns QRPayload or None."""
    if hasattr(_impl, "scan_qr"):
        return await _impl.scan_qr(timeout)  # type: ignore[no-any-return]
    return None


__all__ = [
    "ensure_wifi",
    "gen_uuid",
    "get_device_serial",
    "get_gps_seed",
    "now_ms",
    "read_telemetry",
    "scan_qr",
    "set_gps_seed",
    "wait_for_route",
]
