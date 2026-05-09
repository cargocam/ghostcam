"""Platform-selection test.

Make sure the synthetic platform produces a TelemetryDatagram that
matches the wire types the server expects, and that GHOSTCAM_SYNTHETIC=1
is honored. The real Linux path requires /proc/* sensors and is covered
in the on-Pi smoke test (out of scope here).
"""

from __future__ import annotations

import importlib

import pytest


@pytest.fixture(autouse=True)
def _force_synthetic(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GHOSTCAM_SYNTHETIC", "1")
    # Reload the platform module so it picks up the env override.
    import ghostcam.platform
    importlib.reload(ghostcam.platform)


def test_synthetic_telemetry_has_required_fields() -> None:
    from ghostcam.platform import read_telemetry, set_gps_seed
    set_gps_seed("test-camera-1")
    t = read_telemetry()
    assert t.ts > 0
    assert t.cpu == 15
    assert t.mem == 256
    assert t.temp == 45
    assert t.sig == -55
    assert t.gps_fix == 3
    assert 47.0 < (t.lat or 0) < 48.0  # Seattle-ish
    assert -123.0 < (t.lon or 0) < -122.0


def test_synthetic_gps_seed_is_deterministic() -> None:
    from ghostcam.platform import read_telemetry, set_gps_seed

    set_gps_seed("camera-A")
    a1 = read_telemetry()
    set_gps_seed("camera-A")
    a2 = read_telemetry()
    # Same seed at the same wall-clock moment → same lat/lon offsets.
    # (ts differs by microseconds; we check the offset magnitude.)
    assert abs((a1.lat or 0) - (a2.lat or 0)) < 0.01


def test_synthetic_serial_generation(tmp_path) -> None:  # type: ignore[no-untyped-def]
    from ghostcam.platform import get_device_serial

    # First call generates a UUID-shaped serial and writes it.
    s1 = get_device_serial(tmp_path)
    assert (tmp_path / "device_serial").exists()
    # Second call returns the same persisted serial.
    s2 = get_device_serial(tmp_path)
    assert s1 == s2
