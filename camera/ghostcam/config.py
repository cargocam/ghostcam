"""Camera configuration: defaults → TOML file → env vars → CLI flags.

Mirrors camera/config.go. Search order for the TOML file matches the Go
implementation: --config CLI flag → GHOSTCAM_CONFIG_FILE → {dataDir}/camera.toml
→ /boot/ghostcam.conf.

Two persisted-runtime overrides land here, written by command handlers:
{dataDir}/recording_mode and {dataDir}/resolution. The Go version reads them
in LoadConfig, and so does this one — keeping the contract identical so a
camera that swaps from Go to Python keeps the same behavior.
"""

from __future__ import annotations

import argparse
import logging
import os
import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


@dataclass
class CameraConfig:
    server_url: str
    provision_token: str
    test_source: bool
    segment_dir: Path
    data_dir: Path
    no_gps: bool
    no_audio: bool
    audio_device: str
    video_width: int
    video_height: int
    video_fps: int
    video_bitrate: int
    video_keyframe_interval: int
    recording_mode: str
    local_storage_cap_bytes: int


_VIDEO_PROFILES = {
    "zero2w":  (854, 480, 750_000, 30),
    "480p":    (854, 480, 750_000, 30),
    "pi4":     (1280, 720, 2_000_000, 30),
    "720p":    (1280, 720, 2_000_000, 30),
    "pi5":     (1920, 1080, 4_000_000, 30),
    "1080p":   (1920, 1080, 4_000_000, 30),
}


def _resolve_video_profile(profile: str) -> tuple[int, int, int, int]:
    p = _VIDEO_PROFILES.get(profile)
    if p is None:
        if profile:
            logger.warning("unknown video profile, ignoring: %s", profile)
        return (0, 0, 0, 0)
    logger.info("applying video profile: %s", profile)
    return p


def _coalesce_str(*vals: str | None) -> str:
    for v in vals:
        if v:
            return v
    return ""


def _coalesce_int(*vals: int | None) -> int:
    for v in vals:
        if v:
            return v
    return 0


def _env(key: str) -> str:
    return os.environ.get(key, "")


def _env_int(key: str) -> int:
    s = _env(key)
    if not s:
        return 0
    try:
        return int(s)
    except ValueError:
        logger.warning("could not parse env var %s=%s, ignoring", key, s)
        return 0


def _read_stored_file(data_dir: Path, name: str) -> str:
    try:
        return (data_dir / name).read_text().strip()
    except OSError:
        return ""


def write_stored_file(data_dir: Path, name: str, value: str) -> None:
    """Persist a runtime override to data_dir.

    Mirrors camera/config.go::WriteStoredFile.
    """
    data_dir.mkdir(parents=True, exist_ok=True)
    (data_dir / name).write_text(value)


def _load_config_file(cli_path: str) -> dict[str, Any]:
    candidates = [
        cli_path,
        _env("GHOSTCAM_CONFIG_FILE"),
    ]
    if dd := _env("GHOSTCAM_DATA_DIR"):
        candidates.append(str(Path(dd) / "camera.toml"))
    candidates.append("/boot/ghostcam.conf")

    for p in candidates:
        if not p:
            continue
        path = Path(p)
        if not path.is_file():
            continue
        logger.info("loading config file: %s", p)
        try:
            with path.open("rb") as f:
                return tomllib.load(f)
        except (OSError, tomllib.TOMLDecodeError) as e:
            logger.warning("failed to parse config file %s: %s", p, e)
    return {}


def load_config(argv: list[str] | None = None) -> CameraConfig:
    parser = argparse.ArgumentParser(prog="ghostcam-camera", add_help=True)
    parser.add_argument("--config", default="", help="path to TOML config file")
    parser.add_argument("--server-url", default="", help="server HTTPS URL")
    parser.add_argument("--provision-token", default="", help="one-time provisioning token")
    parser.add_argument("--test-source", action="store_true",
                        help="use ffmpeg test source instead of real capture")
    parser.add_argument("--segment-dir", default="", help="directory for segment ring buffer")
    parser.add_argument("--data-dir", default="", help="data directory")
    parser.add_argument("--no-gps", action="store_true", help="disable GPS")
    parser.add_argument("--no-audio", action="store_true", help="disable audio capture")
    parser.add_argument("--audio-device", default="", help="ALSA audio device name")
    args = parser.parse_args(argv)

    file = _load_config_file(args.config)

    data_dir = Path(_coalesce_str(
        args.data_dir,
        _env("GHOSTCAM_DATA_DIR"),
        file.get("data_dir"),
        "/var/ghostcam",
    ))

    server_url = _coalesce_str(
        args.server_url,
        _env("GHOSTCAM_SERVER_URL"),
        file.get("server_url"),
        _read_stored_file(data_dir, "server_url"),
    )

    provision_token = _coalesce_str(
        args.provision_token,
        _env("GHOSTCAM_PROVISION_TOKEN"),
    )

    test_source = bool(args.test_source) or bool(file.get("test_source"))

    segment_dir = Path(_coalesce_str(
        args.segment_dir,
        _env("GHOSTCAM_SEGMENT_DIR"),
        file.get("segment_dir"),
        str(data_dir / "segments"),
    ))

    no_gps = bool(args.no_gps) or bool(file.get("no_gps"))

    pw, ph, pbr, pkf = _resolve_video_profile(_env("GHOSTCAM_VIDEO_PROFILE"))

    video_width = _coalesce_int(
        _env_int("GHOSTCAM_VIDEO_WIDTH"),
        pw,
        file.get("video_width"),
        1280,
    )
    video_height = _coalesce_int(
        _env_int("GHOSTCAM_VIDEO_HEIGHT"),
        ph,
        file.get("video_height"),
        720,
    )
    video_fps = _coalesce_int(
        _env_int("GHOSTCAM_VIDEO_FPS"),
        0,
        file.get("video_fps"),
        30,
    )
    video_bitrate = _coalesce_int(
        _env_int("GHOSTCAM_VIDEO_BITRATE"),
        pbr,
        file.get("video_bitrate"),
        2_000_000,
    )
    video_keyframe_interval = _coalesce_int(
        _env_int("GHOSTCAM_VIDEO_KEYFRAME_INTERVAL"),
        pkf,
        file.get("video_keyframe_interval"),
        30,
    )

    no_audio = bool(args.no_audio)
    audio_device = _coalesce_str(args.audio_device, _env("GHOSTCAM_AUDIO_DEVICE"))

    if stored := _read_stored_file(data_dir, "resolution"):
        sw, sh, sbr, skf = _resolve_video_profile(stored)
        if sw > 0:
            logger.info("applying stored resolution override: %s", stored)
            video_width, video_height, video_bitrate, video_keyframe_interval = sw, sh, sbr, skf

    recording_mode = "never"
    if env_mode := _env("GHOSTCAM_RECORDING_MODE"):
        recording_mode = env_mode
    if stored_mode := _read_stored_file(data_dir, "recording_mode"):
        recording_mode = stored_mode
        logger.info("applying stored recording mode override: %s", stored_mode)

    local_storage_cap_bytes = 4 * 1024 * 1024 * 1024
    if v := _env("GHOSTCAM_LOCAL_STORAGE_CAP_MB"):
        try:
            mb = int(v)
            if mb > 0:
                local_storage_cap_bytes = mb * 1024 * 1024
        except ValueError:
            pass

    if not str(data_dir):
        raise ValueError("data directory must not be empty")

    return CameraConfig(
        server_url=server_url,
        provision_token=provision_token,
        test_source=test_source,
        segment_dir=segment_dir,
        data_dir=data_dir,
        no_gps=no_gps,
        no_audio=no_audio,
        audio_device=audio_device,
        video_width=video_width,
        video_height=video_height,
        video_fps=video_fps,
        video_bitrate=video_bitrate,
        video_keyframe_interval=video_keyframe_interval,
        recording_mode=recording_mode,
        local_storage_cap_bytes=local_storage_cap_bytes,
    )
