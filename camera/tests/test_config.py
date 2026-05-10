"""Config layering test — defaults → TOML → env → CLI flags.

Mirrors what camera/config_test.go covers in Go: the precedence stack,
video profile expansion, and stored runtime overrides.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from ghostcam.config import load_config


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch) -> None:
    # Drop every GHOSTCAM_* env var so tests start from defaults.
    for k in list(__import__("os").environ):
        if k.startswith("GHOSTCAM_"):
            monkeypatch.delenv(k, raising=False)


def test_defaults_only(tmp_path: Path) -> None:
    cfg = load_config([
        "--data-dir", str(tmp_path),
        "--segment-dir", str(tmp_path / "segments"),
    ])
    assert cfg.video_width == 1280
    assert cfg.video_height == 720
    assert cfg.video_fps == 30
    assert cfg.video_bitrate == 2_000_000
    assert cfg.recording_mode == "never"
    assert cfg.local_storage_cap_bytes == 4 * 1024 * 1024 * 1024


def test_env_overrides_defaults(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("GHOSTCAM_VIDEO_WIDTH", "640")
    monkeypatch.setenv("GHOSTCAM_VIDEO_HEIGHT", "480")
    monkeypatch.setenv("GHOSTCAM_RECORDING_MODE", "motion")
    cfg = load_config(["--data-dir", str(tmp_path)])
    assert cfg.video_width == 640
    assert cfg.video_height == 480
    assert cfg.recording_mode == "motion"


def test_cli_overrides_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("GHOSTCAM_SERVER_URL", "https://from-env.example")
    cfg = load_config([
        "--data-dir", str(tmp_path),
        "--server-url", "https://from-cli.example",
    ])
    assert cfg.server_url == "https://from-cli.example"


def test_video_profile_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("GHOSTCAM_VIDEO_PROFILE", "zero2w")
    cfg = load_config(["--data-dir", str(tmp_path)])
    assert cfg.video_width == 854
    assert cfg.video_height == 480
    assert cfg.video_bitrate == 750_000


def test_stored_recording_mode_overrides_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    # The Go camera persists set_recording_mode commands to disk; that
    # file should win over GHOSTCAM_RECORDING_MODE because the server-
    # commanded value is the source of truth for installed cameras.
    (tmp_path / "recording_mode").write_text("constant")
    monkeypatch.setenv("GHOSTCAM_RECORDING_MODE", "never")
    cfg = load_config(["--data-dir", str(tmp_path)])
    assert cfg.recording_mode == "constant"


def test_toml_file_loaded(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    config_path = tmp_path / "camera.toml"
    config_path.write_text(
        'server_url = "https://from-toml.example"\n'
        'video_width = 320\n'
        'video_height = 240\n'
    )
    cfg = load_config([
        "--data-dir", str(tmp_path),
        "--config", str(config_path),
    ])
    assert cfg.server_url == "https://from-toml.example"
    assert cfg.video_width == 320
    assert cfg.video_height == 240


def test_local_storage_cap_env_in_mb(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("GHOSTCAM_LOCAL_STORAGE_CAP_MB", "256")
    cfg = load_config(["--data-dir", str(tmp_path)])
    assert cfg.local_storage_cap_bytes == 256 * 1024 * 1024
