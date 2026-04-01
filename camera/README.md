# camera

Ghostcam camera agent. Connects to the server via HTTP API, performs enrollment on first boot, then continuously records H.264 video as MPEG-TS segments using `rpicam-vid | ffmpeg` and uploads them to S3 via presigned URLs. Reports system telemetry to the server.

Two modes:
- **Test source** (`--test-source`): uses `ffmpeg` with `testsrc2` to generate test pattern segments. No Pi hardware required. Used for development and Docker.
- **Real capture** (default): `rpicam-vid` piped to `ffmpeg` for MPEG-TS segment generation. Requires Linux with a Pi camera and ffmpeg installed.

## System Requirements

- `ffmpeg` in PATH (both modes)
- `rpicam-vid` in PATH (real capture mode only, Raspberry Pi OS)
- Optional: `gpsd` on `localhost:2947` for GPS

## Configuration

Supports TOML config files, environment variables, and CLI flags with layered resolution: defaults -> config file -> env vars -> CLI flags. Config files are optional.

### Config File Search Order

1. `--config <path>` CLI flag
2. `$GHOSTCAM_CONFIG_FILE` (env var)
3. `$GHOSTCAM_DATA_DIR/camera.toml`
4. `/boot/ghostcam.conf` (backward compatible -- valid TOML key=value format)

See `camera.example.toml` in the repo root for all available settings with comments.

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | _(none)_ | Path to TOML config file |
| `--server-addr` | _(from config / enrollment)_ | Server address |
| `--test-source` | off | Use ffmpeg testsrc2 test pattern |
| `--test-video` | `test-data/test.h264` | H.264 file (legacy, unused with new pipeline) |
| `--data-dir` | `/var/ghostcam` | Device identity and enrollment state |
| `--segment-dir` | `/var/ghostcam/segments` | MPEG-TS segment output directory |
| `--no-audio` | off | Disable audio capture |
| `--no-gps` | off | Disable GPS via gpsd |
| `--no-tofu` | off | Disable TOFU server fingerprint verification (dev/testing) |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | _(none)_ | Explicit path to TOML config file |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory |
| `GHOSTCAM_SERVER_ADDR` | _(from enrollment)_ | Server address |
| `GHOSTCAM_AUDIO_DEVICE` | _(system default)_ | ALSA audio input device name (Linux only) |

Server address resolution precedence: `--server-addr` CLI flag -> `GHOSTCAM_SERVER_ADDR` env var -> config file -> `server.addr` file (written during enrollment) -> hardcoded default.

## How It Works

1. **Provisioning** -- On first boot (no stored credentials), camera enters provisioning mode. Scans QR codes via `rpicam-still` + `rqrr` (Linux only), or uses a pre-provisioned token file (Docker). The server returns an API key and device ID.
2. **Capture pipeline** -- Spawns `rpicam-vid | ffmpeg` (real hardware) or `ffmpeg -f lavfi -i testsrc2` (test mode). ffmpeg writes 6-second MPEG-TS segments to the segment directory (`seg00000.ts`, `seg00001.ts`, ...).
3. **Segment watcher** -- Polls the segment directory every 2 seconds. When a new `.ts` file appears and is at least 1 second old (to ensure ffmpeg has finished writing), it is queued for upload.
4. **Upload loop** -- Requests presigned PUT URLs from the server, uploads `.ts` segments to S3, confirms uploads. Failed uploads are re-queued. Oldest segments are evicted when the queue exceeds capacity (500 segments).
5. **Telemetry** -- Independently polls system sensors every 10 seconds, POSTs to the server. Server responses may include commands (reboot, network config, etc.).

## Architecture

```
rpicam-vid --codec h264 --inline -t 0 -o - |
ffmpeg -i pipe:0 -c copy -f segment -segment_time 6 -reset_timestamps 1 seg%05d.ts

                        segment_dir/
                        seg00000.ts
                        seg00001.ts   <-- watcher detects new files
                        seg00002.ts
                            |
                     segment watcher (2s poll)
                            |
                     upload queue (mpsc)
                            |
                     upload loop --> S3 presigned PUT
```

## Module Map

| Module | Purpose |
|--------|---------|
| `main` | CLI, capture pipeline, segment watcher, task orchestration |
| `config` | `CameraConfig` + `CameraConfigFile`, layered TOML/env/CLI resolution |
| `upload` | Upload queue + S3 presigned URL upload loop |
| `http_client` | HTTP client for server API (telemetry, presign, provision) |
| `provisioning` | QR scan / token-based provisioning flow |
| `qr_enrollment` | QR code scanning (rpicam-still + rqrr, Linux only) |
| `network` | WiFi/NM helpers, `wait_for_route()` |
| `firmware` | Firmware update check on startup |
| `telemetry/sensors` | Platform readers (`/proc/stat`, `/sys/class/thermal`, gpsd, etc.) |
| `telemetry/buffer` | Buffered telemetry for upload after reconnect |
