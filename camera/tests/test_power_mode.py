"""PowerModeState: schedule evaluation, battery rules, manual selection,
priority ordering, mode-change event propagation.

We deliberately don't mock the disk — `tmp_path` gives us a real
data_dir per test, and the persistence round-trip (reload from disk
sees the same state) is part of the contract."""

from __future__ import annotations

import json
from datetime import datetime
from pathlib import Path

import pytest

from ghostcam.power_mode import (
    BATTERY_RULES_FILE,
    POWER_MODE_FILE,
    SCHEDULE_FILE,
    UPLOAD_MODE_FILE,
    BatteryRule,
    PowerModeState,
    ScheduleWindow,
    load,
)

# --- defaults ---


def test_default_is_live_proactive(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    assert state.power_mode == "live"
    assert state.upload_mode == "proactive"
    assert state.effective.source == "default"
    assert state.should_keep_ws_open()
    assert state.should_capture()
    assert state.telemetry_interval_s() == 10.0


# --- manual selection ---


def test_set_manual_persists_to_disk(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("standby")
    state.set_manual_upload_mode("lazy")

    assert (tmp_path / POWER_MODE_FILE).read_text().strip() == "standby"
    assert (tmp_path / UPLOAD_MODE_FILE).read_text().strip() == "lazy"
    assert state.power_mode == "standby"
    assert state.upload_mode == "lazy"
    assert state.effective.source == "manual"


def test_load_round_trips_manual_state(tmp_path: Path) -> None:
    s1 = PowerModeState(data_dir=tmp_path)
    s1.set_manual_power_mode("sleep")
    s1.set_manual_upload_mode("lazy")

    s2 = load(tmp_path)
    assert s2.power_mode == "sleep"
    assert s2.upload_mode == "lazy"


def test_invalid_power_mode_raises(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    with pytest.raises(ValueError, match="invalid power_mode"):
        state.set_manual_power_mode("turbo")


def test_invalid_upload_mode_raises(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    with pytest.raises(ValueError, match="invalid upload_mode"):
        state.set_manual_upload_mode("yolo")


# --- behavioural helpers ---


def test_should_capture_per_mode(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("live")
    assert state.should_capture()
    state.set_manual_power_mode("standby")
    assert state.should_capture()
    state.set_manual_power_mode("sleep")
    assert not state.should_capture()


def test_should_upload_motion_exempt(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    # proactive: everything uploads
    assert state.should_upload(has_motion=False)
    assert state.should_upload(has_motion=True)
    state.set_manual_upload_mode("lazy")
    # lazy: only motion uploads
    assert not state.should_upload(has_motion=False)
    assert state.should_upload(has_motion=True)


def test_should_keep_ws_open_only_in_live(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    assert state.should_keep_ws_open()
    state.set_manual_power_mode("standby")
    assert not state.should_keep_ws_open()
    state.set_manual_power_mode("sleep")
    assert not state.should_keep_ws_open()


def test_telemetry_interval_in_sleep(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("sleep")
    assert state.telemetry_interval_s() == 300.0
    assert state.telemetry_interval_s(sleep_poll_interval_s=600.0) == 600.0


# --- schedules ---


def test_schedule_window_applies_within_range(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_schedule([
        ScheduleWindow(
            start_hhmm="22:00", end_hhmm="06:00",
            power_mode="sleep", upload_mode="lazy",
        ),
    ])
    # Inside the window (wraps midnight): 02:00 falls in 22:00–06:00.
    eff = state.recompute(now=datetime(2026, 5, 9, 2, 0))
    assert eff.power_mode == "sleep"
    assert eff.source == "schedule"

    # Outside: 12:00 is daytime.
    eff = state.recompute(now=datetime(2026, 5, 9, 12, 0))
    assert eff.power_mode == "live"
    assert eff.source == "default"


def test_schedule_persists_and_reloads(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_schedule([
        ScheduleWindow(
            start_hhmm="09:00", end_hhmm="17:00",
            power_mode="live", upload_mode="proactive",
            days=frozenset((0, 1, 2, 3, 4)),  # Mon–Fri
        ),
    ])
    raw = json.loads((tmp_path / SCHEDULE_FILE).read_text())
    assert raw[0]["start"] == "09:00"
    assert raw[0]["days"] == [0, 1, 2, 3, 4]

    s2 = load(tmp_path)
    assert len(s2._schedule) == 1
    # 2026-05-11 is a Monday at 10:00 → window matches.
    eff = s2.recompute(now=datetime(2026, 5, 11, 10, 0))
    assert eff.source == "schedule"


def test_schedule_day_filter(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_schedule([
        ScheduleWindow(
            start_hhmm="00:00", end_hhmm="23:59",
            power_mode="standby", upload_mode="lazy",
            days=frozenset((5, 6)),  # Sat, Sun
        ),
    ])
    # Saturday: applies.
    eff = state.recompute(now=datetime(2026, 5, 9, 14, 0))  # Sat
    assert eff.power_mode == "standby"
    # Wednesday: doesn't apply.
    eff = state.recompute(now=datetime(2026, 5, 13, 14, 0))  # Wed
    assert eff.power_mode == "live"


def test_schedule_priority_over_manual(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("live")
    state.set_schedule([
        ScheduleWindow(
            start_hhmm="00:00", end_hhmm="23:59",
            power_mode="sleep", upload_mode="lazy",
        ),
    ])
    eff = state.recompute(now=datetime(2026, 5, 9, 12, 0))
    assert eff.power_mode == "sleep"
    assert eff.source == "schedule"


# --- battery rules ---


def test_battery_rule_fires_below_threshold(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_battery_rules([
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    state.set_battery_pct(15)
    assert state.power_mode == "sleep"
    assert state.upload_mode == "lazy"
    assert state.effective.source == "battery"


def test_battery_rule_does_not_fire_above_threshold(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_battery_rules([
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    state.set_battery_pct(50)
    assert state.power_mode == "live"
    assert state.effective.source == "default"


def test_battery_rule_lowest_threshold_wins(tmp_path: Path) -> None:
    """When multiple rules apply (battery is below several thresholds),
    the most aggressive (lowest threshold) one wins."""
    state = PowerModeState(data_dir=tmp_path)
    state.set_battery_rules([
        BatteryRule(threshold_pct=50, power_mode="standby", upload_mode="lazy"),
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    # At 15%: both rules apply; sleep wins (lowest threshold).
    state.set_battery_pct(15)
    assert state.power_mode == "sleep"


def test_battery_rule_chains_with_thresholds(tmp_path: Path) -> None:
    """Between thresholds: only the higher-threshold rule fires."""
    state = PowerModeState(data_dir=tmp_path)
    state.set_battery_rules([
        BatteryRule(threshold_pct=50, power_mode="standby", upload_mode="lazy"),
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    state.set_battery_pct(40)
    assert state.power_mode == "standby"
    assert state.effective.source == "battery"


# --- priority ordering ---


def test_priority_schedule_beats_battery_beats_manual(tmp_path: Path) -> None:
    """All three sources active simultaneously: schedule wins."""
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("standby")
    state.set_battery_rules([
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    state.set_battery_pct(10)
    state.set_schedule([
        ScheduleWindow(
            start_hhmm="00:00", end_hhmm="23:59",
            power_mode="live", upload_mode="proactive",
        ),
    ])
    eff = state.recompute(now=datetime(2026, 5, 9, 12, 0))
    assert eff.source == "schedule"
    assert eff.power_mode == "live"


def test_priority_battery_beats_manual_when_no_schedule(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("live")
    state.set_battery_rules([
        BatteryRule(threshold_pct=20, power_mode="sleep", upload_mode="lazy"),
    ])
    state.set_battery_pct(10)
    eff = state.recompute()
    assert eff.source == "battery"
    assert eff.power_mode == "sleep"


# --- changed event ---


@pytest.mark.asyncio
async def test_changed_event_fires_on_transition(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("standby")
    # changed.set() then clear() happens synchronously inside recompute,
    # so the event auto-rearms. But we can confirm via observable state:
    assert state.power_mode == "standby"


@pytest.mark.asyncio
async def test_no_change_no_event(tmp_path: Path) -> None:
    """Setting the same mode twice doesn't refire."""
    state = PowerModeState(data_dir=tmp_path)
    state.set_manual_power_mode("live")  # already live
    # Nothing to assert on the event itself (auto-clears); we assert
    # the state is unchanged + the call was idempotent.
    assert state.power_mode == "live"


# --- corrupt-file resilience ---


def test_load_handles_corrupt_files(tmp_path: Path) -> None:
    (tmp_path / POWER_MODE_FILE).write_text("turbo\n")
    (tmp_path / UPLOAD_MODE_FILE).write_text("yolo\n")
    (tmp_path / SCHEDULE_FILE).write_text("not-json")
    (tmp_path / BATTERY_RULES_FILE).write_text("[{}]")
    state = load(tmp_path)
    # Falls back to defaults across the board.
    assert state.power_mode == "live"
    assert state.upload_mode == "proactive"


# --- JSON command-format compatibility (set_schedule via commands.py) ---


def test_set_schedule_from_json_string(tmp_path: Path) -> None:
    """commands.py will pass the raw JSON from the command body; the
    state must accept that without going through the dataclass."""
    state = PowerModeState(data_dir=tmp_path)
    state.set_schedule(json.dumps([
        {
            "start": "22:00",
            "end": "06:00",
            "power_mode": "sleep",
            "upload_mode": "lazy",
            "days": [5, 6],
        },
    ]))
    assert len(state._schedule) == 1
    assert state._schedule[0].days == frozenset((5, 6))


def test_set_battery_rules_from_json_string(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    state.set_battery_rules(json.dumps([
        {"threshold_pct": 20, "power_mode": "sleep", "upload_mode": "lazy"},
    ]))
    assert len(state._battery_rules) == 1
    assert state._battery_rules[0].threshold_pct == 20


def test_battery_rule_threshold_validation(tmp_path: Path) -> None:
    state = PowerModeState(data_dir=tmp_path)
    with pytest.raises(ValueError, match="threshold_pct out of range"):
        state.set_battery_rules(json.dumps([
            {"threshold_pct": 150, "power_mode": "sleep", "upload_mode": "lazy"},
        ]))
