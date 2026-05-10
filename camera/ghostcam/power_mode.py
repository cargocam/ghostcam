"""Power-mode state + schedule + battery-rule resolution.

Two orthogonal axes that the camera evaluates centrally:

  power_mode:  live    — capture + WS + telemetry every 10 s (default)
               standby — capture + telemetry every 10 s, WS only on
                         viewer demand (`wake_live` flag from server)
               sleep   — capture/GPS off, telemetry every 5 min,
                         live unavailable. Recording disabled.

  upload_mode: proactive — every segment uploads ASAP (default)
               lazy      — only motion-flagged segments upload
                           proactively; non-motion wait for an
                           `upload_segments` command from the server.

Selection sources, evaluated in priority order:

  1. Active schedule rule (highest priority)
  2. Battery-driven rule (e.g. battery < 20% → sleep + lazy)
  3. Manually-set mode (set_power_mode / set_upload_mode commands)
  4. Default: live + proactive

`PowerModeState` is the central holder. Long-running tasks
(`telemetry_poll`, `live_ws`, `upload`, `capture` supervisor) read
its `effective` snapshot and subscribe via the `changed`
asyncio.Event so they can react to mode transitions without polling.

Sleep is degenerate against the upload axis (nothing to upload), so
upload_mode is ignored in sleep mode for behaviour purposes; the
field is still tracked so the camera reports it back accurately.
"""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass, field, replace
from datetime import datetime, time
from pathlib import Path
from typing import Any, Literal

logger = logging.getLogger(__name__)

PowerMode = Literal["live", "standby", "sleep"]
UploadMode = Literal["proactive", "lazy"]

VALID_POWER_MODES: frozenset[str] = frozenset(("live", "standby", "sleep"))
VALID_UPLOAD_MODES: frozenset[str] = frozenset(("proactive", "lazy"))

# On-disk filenames under data_dir.
POWER_MODE_FILE = "power_mode"
UPLOAD_MODE_FILE = "upload_mode"
SCHEDULE_FILE = "schedule.json"
BATTERY_RULES_FILE = "battery_rules.json"


# --- schedule rules ---


@dataclass(frozen=True, slots=True)
class ScheduleWindow:
    """A single window in the schedule.

    `days` is a bitmask of weekdays (0=Mon, 6=Sun); empty set means all days.
    `start_hhmm` and `end_hhmm` bracket a wall-clock time-of-day window in
    24h "HH:MM" format. If end < start the window wraps midnight.
    `power_mode` and `upload_mode` are what we apply when this window is active.
    """

    start_hhmm: str
    end_hhmm: str
    power_mode: PowerMode
    upload_mode: UploadMode
    days: frozenset[int] = field(default_factory=frozenset)

    def applies_at(self, when: datetime) -> bool:
        if self.days and when.weekday() not in self.days:
            return False
        cur = when.time()
        start = _parse_hhmm(self.start_hhmm)
        end = _parse_hhmm(self.end_hhmm)
        if start <= end:
            return start <= cur < end
        # Wraps midnight: e.g. 22:00–06:00 means 22:00..24:00 OR 00:00..06:00
        return cur >= start or cur < end


# --- battery rules ---


@dataclass(frozen=True, slots=True)
class BatteryRule:
    """When battery_pct <= threshold_pct, force this mode pair.

    Rules are evaluated in ascending threshold order, so define them with
    the lowest threshold first if multiple should chain.
    """

    threshold_pct: int
    power_mode: PowerMode
    upload_mode: UploadMode


# --- the central state holder ---


@dataclass(frozen=True, slots=True)
class EffectiveMode:
    """Result of applying schedule / battery / manual selection."""

    power_mode: PowerMode
    upload_mode: UploadMode
    source: str  # "schedule" | "battery" | "manual" | "default"


def _parse_hhmm(s: str) -> time:
    h, m = s.split(":", 1)
    return time(hour=int(h), minute=int(m))


def _read_first_line(path: Path) -> str:
    try:
        return path.read_text().strip()
    except OSError:
        return ""


def _atomic_write_text(path: Path, content: str) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(content)
    tmp.replace(path)


def _validate_power_mode(value: str) -> PowerMode:
    if value not in VALID_POWER_MODES:
        raise ValueError(f"invalid power_mode: {value!r}")
    return value  # type: ignore[return-value]


def _validate_upload_mode(value: str) -> UploadMode:
    if value not in VALID_UPLOAD_MODES:
        raise ValueError(f"invalid upload_mode: {value!r}")
    return value  # type: ignore[return-value]


class PowerModeState:
    """Central mode state. Persisted to disk, reactive on change.

    Intended lifecycle:
      * `load(data_dir)` at boot — pulls manual-mode + schedule + battery
        rules from disk.
      * `set_manual_*()` write to disk and recompute effective mode.
      * `set_battery_pct()` triggers battery-rule re-evaluation.
      * `recompute()` is also called periodically by a background task to
        catch schedule transitions (every 60 s is enough).
      * `changed` asyncio.Event fires on every effective-mode transition.

    The class is intentionally not async-only — `set_*` methods are sync
    so callers (commands.py) don't need to await for the disk write.
    `changed` is the async coordination point.
    """

    def __init__(
        self,
        data_dir: Path,
        manual_power_mode: PowerMode = "live",
        manual_upload_mode: UploadMode = "proactive",
        schedule: list[ScheduleWindow] | None = None,
        battery_rules: list[BatteryRule] | None = None,
    ) -> None:
        self._data_dir = data_dir
        self._manual_power_mode: PowerMode = manual_power_mode
        self._manual_upload_mode: UploadMode = manual_upload_mode
        self._schedule: list[ScheduleWindow] = list(schedule or [])
        self._battery_rules: list[BatteryRule] = sorted(
            battery_rules or [], key=lambda r: r.threshold_pct,
        )
        self._battery_pct: int | None = None
        self._effective: EffectiveMode = self._compute(datetime.now())
        # Async coordination — async tasks await `changed` to react to
        # mode transitions. We call set() inside recompute() whenever
        # effective changes.
        self.changed: asyncio.Event = asyncio.Event()

    # --- snapshots ---

    @property
    def effective(self) -> EffectiveMode:
        return self._effective

    @property
    def power_mode(self) -> PowerMode:
        return self._effective.power_mode

    @property
    def upload_mode(self) -> UploadMode:
        return self._effective.upload_mode

    def telemetry_interval_s(self, sleep_poll_interval_s: float = 300.0) -> float:
        """The poll cadence for the current power mode.

        live/standby use the camera's normal 10 s base interval (caller
        applies its own backoff on consecutive failures). Sleep stretches
        to a configurable interval (default 5 min).
        """
        if self._effective.power_mode == "sleep":
            return sleep_poll_interval_s
        return 10.0

    def should_capture(self) -> bool:
        return self._effective.power_mode != "sleep"

    def should_upload(self, has_motion: bool) -> bool:
        """Per-segment decision used by upload.py.

        Motion-flagged segments always upload (preserves real-time alerts).
        Non-motion segments only upload in proactive mode. Sleep mode
        doesn't record so this never fires there.
        """
        if has_motion:
            return True
        return self._effective.upload_mode == "proactive"

    def should_keep_ws_open(self) -> bool:
        """live mode keeps the WS open; standby opens on demand; sleep
        skips it entirely."""
        return self._effective.power_mode == "live"

    # --- mutation ---

    def set_manual_power_mode(self, mode: PowerMode | str) -> None:
        validated = _validate_power_mode(mode)
        if validated == self._manual_power_mode:
            return
        self._manual_power_mode = validated
        _atomic_write_text(self._data_dir / POWER_MODE_FILE, validated)
        self.recompute()

    def set_manual_upload_mode(self, mode: UploadMode | str) -> None:
        validated = _validate_upload_mode(mode)
        if validated == self._manual_upload_mode:
            return
        self._manual_upload_mode = validated
        _atomic_write_text(self._data_dir / UPLOAD_MODE_FILE, validated)
        self.recompute()

    def set_schedule(self, schedule: list[ScheduleWindow] | str) -> None:
        windows = (
            _parse_schedule(schedule)
            if isinstance(schedule, str)
            else list(schedule)
        )
        self._schedule = windows
        _atomic_write_text(
            self._data_dir / SCHEDULE_FILE,
            json.dumps([_window_to_json(w) for w in windows]),
        )
        self.recompute()

    def set_battery_rules(self, rules: list[BatteryRule] | str) -> None:
        parsed = (
            _parse_battery_rules(rules)
            if isinstance(rules, str)
            else list(rules)
        )
        self._battery_rules = sorted(parsed, key=lambda r: r.threshold_pct)
        _atomic_write_text(
            self._data_dir / BATTERY_RULES_FILE,
            json.dumps([_rule_to_json(r) for r in self._battery_rules]),
        )
        self.recompute()

    def set_battery_pct(self, pct: int | None) -> None:
        if pct == self._battery_pct:
            return
        self._battery_pct = pct
        self.recompute()

    def recompute(self, now: datetime | None = None) -> EffectiveMode:
        previous = self._effective
        new_effective = self._compute(now or datetime.now())
        self._effective = new_effective
        if new_effective != previous:
            logger.info(
                "power_mode transition: %s/%s (%s) -> %s/%s (%s)",
                previous.power_mode, previous.upload_mode, previous.source,
                new_effective.power_mode, new_effective.upload_mode,
                new_effective.source,
            )
            self.changed.set()
            # Auto-reset so awaiters can re-arm. Tasks that need to read
            # the value should snapshot via `effective` after waking.
            self.changed.clear()
        return new_effective

    # --- internal ---

    def _compute(self, now: datetime) -> EffectiveMode:
        # 1. Schedule has the highest priority.
        for window in self._schedule:
            if window.applies_at(now):
                return EffectiveMode(
                    power_mode=window.power_mode,
                    upload_mode=window.upload_mode,
                    source="schedule",
                )

        # 2. Battery rule. Pick the rule with the highest threshold that
        #    we're still under (so a 30% rule fires at 25% AND a 20%
        #    rule fires at 15%; the lowest applicable rule wins because
        #    rules are sorted ascending).
        if self._battery_pct is not None and self._battery_rules:
            applicable = [
                r for r in self._battery_rules
                if self._battery_pct <= r.threshold_pct
            ]
            if applicable:
                # Lowest threshold wins (most aggressive applicable rule).
                rule = applicable[0]
                return EffectiveMode(
                    power_mode=rule.power_mode,
                    upload_mode=rule.upload_mode,
                    source="battery",
                )

        # 3. Manual selection.
        is_default = (
            self._manual_power_mode == "live"
            and self._manual_upload_mode == "proactive"
        )
        return EffectiveMode(
            power_mode=self._manual_power_mode,
            upload_mode=self._manual_upload_mode,
            source="default" if is_default else "manual",
        )


# --- JSON encoding helpers (used by set_schedule / set_battery_rules
# from command JSON bodies) ---


def _window_to_json(w: ScheduleWindow) -> dict[str, Any]:
    out: dict[str, Any] = {
        "start": w.start_hhmm,
        "end": w.end_hhmm,
        "power_mode": w.power_mode,
        "upload_mode": w.upload_mode,
    }
    if w.days:
        out["days"] = sorted(w.days)
    return out


def _window_from_json(obj: dict[str, Any]) -> ScheduleWindow:
    days = frozenset(int(d) for d in obj.get("days", ()))
    return ScheduleWindow(
        start_hhmm=str(obj["start"]),
        end_hhmm=str(obj["end"]),
        power_mode=_validate_power_mode(obj["power_mode"]),
        upload_mode=_validate_upload_mode(obj["upload_mode"]),
        days=days,
    )


def _parse_schedule(s: str) -> list[ScheduleWindow]:
    if not s.strip():
        return []
    raw = json.loads(s)
    if not isinstance(raw, list):
        raise ValueError("schedule must be a JSON array")
    return [_window_from_json(item) for item in raw]


def _rule_to_json(r: BatteryRule) -> dict[str, Any]:
    return {
        "threshold_pct": r.threshold_pct,
        "power_mode": r.power_mode,
        "upload_mode": r.upload_mode,
    }


def _rule_from_json(obj: dict[str, Any]) -> BatteryRule:
    pct = int(obj["threshold_pct"])
    if not 0 <= pct <= 100:
        raise ValueError(f"threshold_pct out of range: {pct}")
    return BatteryRule(
        threshold_pct=pct,
        power_mode=_validate_power_mode(obj["power_mode"]),
        upload_mode=_validate_upload_mode(obj["upload_mode"]),
    )


def _parse_battery_rules(s: str) -> list[BatteryRule]:
    if not s.strip():
        return []
    raw = json.loads(s)
    if not isinstance(raw, list):
        raise ValueError("battery_rules must be a JSON array")
    return [_rule_from_json(item) for item in raw]


# --- boot-time loader ---


def load(data_dir: Path) -> PowerModeState:
    """Read all four pieces of mode state from data_dir and construct a
    PowerModeState. Missing or corrupt files fall back to defaults so a
    fresh data_dir still boots cleanly."""
    data_dir.mkdir(parents=True, exist_ok=True)

    raw_power = _read_first_line(data_dir / POWER_MODE_FILE)
    manual_power: PowerMode = "live"
    if raw_power:
        try:
            manual_power = _validate_power_mode(raw_power)
        except ValueError:
            logger.warning("corrupt power_mode file (%s), using default", raw_power)

    raw_upload = _read_first_line(data_dir / UPLOAD_MODE_FILE)
    manual_upload: UploadMode = "proactive"
    if raw_upload:
        try:
            manual_upload = _validate_upload_mode(raw_upload)
        except ValueError:
            logger.warning("corrupt upload_mode file (%s), using default", raw_upload)

    schedule: list[ScheduleWindow] = []
    sched_path = data_dir / SCHEDULE_FILE
    if sched_path.exists():
        try:
            schedule = _parse_schedule(sched_path.read_text())
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as e:
            logger.warning("corrupt schedule.json (%s), discarding", e)

    battery_rules: list[BatteryRule] = []
    rules_path = data_dir / BATTERY_RULES_FILE
    if rules_path.exists():
        try:
            battery_rules = _parse_battery_rules(rules_path.read_text())
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as e:
            logger.warning("corrupt battery_rules.json (%s), discarding", e)

    return PowerModeState(
        data_dir=data_dir,
        manual_power_mode=manual_power,
        manual_upload_mode=manual_upload,
        schedule=schedule,
        battery_rules=battery_rules,
    )


# Re-export the dataclass for tests and external callers.
__all__ = [
    "BATTERY_RULES_FILE",
    "BatteryRule",
    "EffectiveMode",
    "POWER_MODE_FILE",
    "PowerMode",
    "PowerModeState",
    "SCHEDULE_FILE",
    "ScheduleWindow",
    "UPLOAD_MODE_FILE",
    "UploadMode",
    "VALID_POWER_MODES",
    "VALID_UPLOAD_MODES",
    "load",
]


# silence unused-import warning under static analysis
_ = replace
