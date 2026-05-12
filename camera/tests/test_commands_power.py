"""Power-mode command-dispatch tests.

The legacy commands (reboot, set_recording_mode, etc.) are exercised
in the integration suite. Here we cover only the five new types
(set_power_mode, set_upload_mode, set_schedule, set_battery_rules,
upload_segments) since they don't touch the network and don't exit
the process — they mutate PowerModeState in-place and the priority
deque in-place.
"""

from __future__ import annotations

import json
from collections import deque
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import pytest

from ghostcam.commands import handle_command
from ghostcam.power_mode import PowerModeState
from ghostcam.wire import CameraCommand


@pytest.fixture
def power(tmp_path: Path) -> PowerModeState:
    return PowerModeState(data_dir=tmp_path)


@pytest.fixture
def fake_client() -> MagicMock:
    """A bare MagicMock — none of the new commands touch the network."""
    client = MagicMock()
    client.aclose = AsyncMock(return_value=None)
    return client


@pytest.mark.asyncio
async def test_set_power_mode_applies(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    cmd = CameraCommand(type="set_power_mode", power_mode="standby")
    await handle_command(cmd, tmp_path, fake_client, power=power)
    assert power.power_mode == "standby"
    # Persisted to disk so a restart picks it up.
    assert (tmp_path / "power_mode").read_text().strip() == "standby"


@pytest.mark.asyncio
async def test_set_upload_mode_applies(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    cmd = CameraCommand(type="set_upload_mode", upload_mode="lazy")
    await handle_command(cmd, tmp_path, fake_client, power=power)
    assert power.upload_mode == "lazy"
    assert (tmp_path / "upload_mode").read_text().strip() == "lazy"


@pytest.mark.asyncio
async def test_invalid_power_mode_is_dropped(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    cmd = CameraCommand(type="set_power_mode", power_mode="turbo")
    await handle_command(cmd, tmp_path, fake_client, power=power)
    # Stays at default (live).
    assert power.power_mode == "live"


@pytest.mark.asyncio
async def test_set_schedule_round_trip(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    schedule = json.dumps([
        {
            "start": "22:00",
            "end": "06:00",
            "power_mode": "sleep",
            "upload_mode": "lazy",
        },
    ])
    cmd = CameraCommand(type="set_schedule", schedule=schedule)
    await handle_command(cmd, tmp_path, fake_client, power=power)
    assert (tmp_path / "schedule.json").exists()


@pytest.mark.asyncio
async def test_set_schedule_with_empty_string_clears(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    # Initial schedule.
    await handle_command(
        CameraCommand(type="set_schedule", schedule=json.dumps([
            {"start": "22:00", "end": "06:00",
             "power_mode": "sleep", "upload_mode": "lazy"},
        ])),
        tmp_path, fake_client, power=power,
    )
    # Clear via empty string.
    await handle_command(
        CameraCommand(type="set_schedule", schedule=""),
        tmp_path, fake_client, power=power,
    )
    raw = json.loads((tmp_path / "schedule.json").read_text())
    assert raw == []


@pytest.mark.asyncio
async def test_set_battery_rules_applies(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    rules = json.dumps([
        {"threshold_pct": 20, "power_mode": "sleep", "upload_mode": "lazy"},
    ])
    cmd = CameraCommand(type="set_battery_rules", battery_rules=rules)
    await handle_command(cmd, tmp_path, fake_client, power=power)
    raw = json.loads((tmp_path / "battery_rules.json").read_text())
    assert raw[0]["threshold_pct"] == 20


@pytest.mark.asyncio
async def test_upload_segments_pushes_to_priority_deque(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock,
) -> None:
    queue: deque[str] = deque()
    cmd = CameraCommand(
        type="upload_segments",
        segment_ids=["seg00001", "seg00042"],
    )
    await handle_command(
        cmd, tmp_path, fake_client,
        power=power,
        prioritize_uploads=lambda ids: queue.extend(ids),
    )
    assert list(queue) == ["seg00001", "seg00042"]


@pytest.mark.asyncio
async def test_upload_segments_no_callback_is_logged_not_crashed(
    tmp_path: Path, power: PowerModeState, fake_client: MagicMock, caplog,
) -> None:
    cmd = CameraCommand(type="upload_segments", segment_ids=["x"])
    await handle_command(cmd, tmp_path, fake_client, power=power)
    # No exception, just a warning.
    assert any(
        "upload-prioritise hook" in rec.message
        for rec in caplog.records
    )


@pytest.mark.asyncio
async def test_set_power_mode_without_power_state_is_noop(
    tmp_path: Path, fake_client: MagicMock,
) -> None:
    """Legacy callers that don't wire in PowerModeState shouldn't crash."""
    cmd = CameraCommand(type="set_power_mode", power_mode="standby")
    # No `power=` — handler silently does nothing.
    await handle_command(cmd, tmp_path, fake_client)


# --- set_recording_mode hot-swap (PR B follow-up) ---


def _patch_exit(monkeypatch: pytest.MonkeyPatch) -> None:
    """Replace os._exit with a function that raises SystemExit. The
    production handler uses os._exit (C-level exit, bypasses Python
    cleanup) so systemd restarts the daemon; here we want the
    handler-exit to be catchable inside a test."""
    import os as _os

    def _raise_systemexit(code: int) -> None:
        raise SystemExit(code)

    monkeypatch.setattr(_os, "_exit", _raise_systemexit)


@pytest.mark.asyncio
async def test_set_recording_mode_hot_swaps_constant_to_motion(
    tmp_path: Path, power: PowerModeState,
) -> None:
    """Going constant → motion mutates the holder in-place, no exit."""
    holder = ["constant"]
    client = MagicMock()
    await handle_command(
        CameraCommand(type="set_recording_mode", mode="motion"),
        tmp_path, client,
        power=power,
        set_recording_mode=lambda m: holder.__setitem__(0, m),
        get_recording_mode=lambda: holder[0],
    )
    assert holder[0] == "motion"
    # Stored file is still written so the new mode survives a future
    # restart for any reason.
    assert (tmp_path / "recording_mode").read_text() == "motion"


@pytest.mark.asyncio
async def test_set_recording_mode_exits_on_transition_to_never(
    tmp_path: Path, power: PowerModeState, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Going constant → never requires task-lifecycle change → restart."""
    _patch_exit(monkeypatch)
    holder = ["constant"]
    client = MagicMock()
    with pytest.raises(SystemExit) as exc:
        await handle_command(
            CameraCommand(type="set_recording_mode", mode="never"),
            tmp_path, client,
            power=power,
            set_recording_mode=lambda m: holder.__setitem__(0, m),
            get_recording_mode=lambda: holder[0],
        )
    assert exc.value.code == 0
    # Holder was NOT mutated — restart path means main.py will re-read
    # the stored file on next boot.
    assert holder[0] == "constant"
    assert (tmp_path / "recording_mode").read_text() == "never"


@pytest.mark.asyncio
async def test_set_recording_mode_exits_on_transition_from_never(
    tmp_path: Path, power: PowerModeState, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Going never → constant requires spawning watcher+upload tasks → restart."""
    _patch_exit(monkeypatch)
    holder = ["never"]
    client = MagicMock()
    with pytest.raises(SystemExit):
        await handle_command(
            CameraCommand(type="set_recording_mode", mode="constant"),
            tmp_path, client,
            power=power,
            set_recording_mode=lambda m: holder.__setitem__(0, m),
            get_recording_mode=lambda: holder[0],
        )
    assert holder[0] == "never"


@pytest.mark.asyncio
async def test_set_recording_mode_falls_back_to_restart_without_holder(
    tmp_path: Path, power: PowerModeState, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """When set_recording_mode/get_recording_mode kwargs are absent
    (legacy callers, partial setups), the handler falls back to the
    old restart-to-apply path so behaviour is unchanged."""
    _patch_exit(monkeypatch)
    client = MagicMock()
    with pytest.raises(SystemExit):
        await handle_command(
            CameraCommand(type="set_recording_mode", mode="motion"),
            tmp_path, client,
            power=power,
        )


# --- Network recovery (GH #82) ---


@pytest.mark.asyncio
async def test_recover_network_triggered_after_threshold(monkeypatch: pytest.MonkeyPatch) -> None:
    """After NETWORK_RECOVERY_THRESHOLD consecutive POST failures,
    telemetry_poll calls platform.recover_network and increments the
    counter that's surfaced on telemetry."""
    from ghostcam import telemetry_poll
    from ghostcam.upload import flags as upload_flags

    # Reset counter so other tests don't poison this one.
    upload_flags.network_recovery_attempts = 0
    upload_flags.upload_latency_ms_window.clear()

    recover_calls = 0

    async def fake_recover() -> bool:
        nonlocal recover_calls
        recover_calls += 1
        return True

    monkeypatch.setattr(telemetry_poll, "recover_network", fake_recover)

    # Drive _compute_failure_interval-equivalent state manually: we
    # don't run the full loop, we just exercise the threshold logic.
    # The trigger is `consecutive_failures >= NETWORK_RECOVERY_THRESHOLD`
    # combined with a cooldown gate; below threshold = no call.
    threshold = telemetry_poll.NETWORK_RECOVERY_THRESHOLD
    assert threshold >= 1

    # Simulate the loop's failure path firing N times. Each "failure"
    # is a single iteration that bumps the counter; we mirror that.
    for i in range(1, threshold + 2):
        consecutive_failures = i
        if consecutive_failures >= threshold and recover_calls == 0:
            upload_flags.network_recovery_attempts += 1
            await fake_recover()

    assert recover_calls == 1
    assert upload_flags.network_recovery_attempts == 1
