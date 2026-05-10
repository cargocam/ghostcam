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
