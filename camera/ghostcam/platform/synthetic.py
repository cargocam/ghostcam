"""Synthetic-sensor stubs.

Mirrors camera/sensors_other.go. Used in Docker compose `--profile test`,
in CI integration tests, and on dev machines where /proc isn't available.
The synthetic GPS orbit clusters cameras deterministically based on a
seed (the device serial), exactly like the Go camera does — so docker
compose's three test cameras land at the same map positions whether the
camera is Go or Python.
"""

from __future__ import annotations

import math
import time
from pathlib import Path

from ghostcam.wire import TelemetryDatagram

_GPS_SEED = ""

# Three slot offsets — slot 0/1 cluster within ~1km, slot 2 is ~20km away.
_SLOT_OFFSETS: list[tuple[float, float]] = [
    (0.002, 0.003),
    (0.004, 0.001),
    (0.15, -0.10),
]


def set_gps_seed(seed: str) -> None:
    global _GPS_SEED
    _GPS_SEED = seed


def read_device_serial(data_dir: Path) -> str:
    """Synthetic platform doesn't have a hardware serial; return empty so
    the caller generates a UUID."""
    return ""


def read_telemetry() -> TelemetryDatagram:
    h = 0
    for b in _GPS_SEED.encode():
        h = h * 31 + b
    phase_offset = (h % 10000) / 10000.0 * 2 * math.pi
    slot = h % 3
    lat_offset, lon_offset = _SLOT_OFFSETS[slot]

    t = time.time()
    lat = 47.6062 + lat_offset + 0.005 * math.sin(t / 120.0 + phase_offset)
    lon = -122.3321 + lon_offset + 0.005 * math.cos(t / 90.0 + phase_offset)

    uptime = int(time.time()) % 86400
    return TelemetryDatagram(
        ts=int(time.time() * 1000),
        cpu=15,
        mem=256,
        temp=45,
        uptime=uptime,
        sig=-55,
        lat=lat,
        lon=lon,
        alt=50.0,
        gps_fix=3,
    )


# wait_for_route / ensure_wifi / scan_qr intentionally absent — the
# synthetic platform is a no-op for those (handled by the package
# __init__.py fallbacks).
