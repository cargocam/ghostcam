# camera

Ghostcam camera agent. Connects to the server over QUIC with mTLS, performs enrollment on first boot, then continuously streams H.264 video, Opus audio, and system telemetry. Records video locally to an fMP4 ring buffer for HLS playback.

Two modes:
- **Test source** (`--test-source`): loops a pre-recorded H.264 file with synthetic audio. No system dependencies beyond Rust. Used for development and Docker.
- **Real capture** (default): `rpicam-vid`/`libcamera-vid` for video, `cpal` + Opus for audio, `/proc`/`/sys` + gpsd for telemetry. Requires Linux with a camera.

Auto-reconnects to the server with exponential backoff (1s → 30s).

## System Requirements (Real Capture)

- `rpicam-vid` or `libcamera-vid` in PATH (Raspberry Pi OS or libcamera-enabled Linux)
- `libopus-dev` (Linux) or `brew install opus` (macOS, cross-compilation)
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
| `--server-addr` | _(from config / enrollment)_ | Server QUIC address `host:port` |
| `--test-source` | off | Use test H.264 file + synthetic audio |
| `--test-video` | `test-data/test.h264` | H.264 file for `--test-source` |
| `--data-dir` | `/var/ghostcam` | Device cert, config, and enrollment state |
| `--segment-dir` | `/var/ghostcam/segments` | fMP4 ring buffer for HLS recording |
| `--no-audio` | off | Disable audio capture |
| `--no-gps` | off | Disable GPS via gpsd |
| `--enrollment-jwt` | _(none)_ | JWT for enrollment |
| `--no-tofu` | off | Disable TOFU server fingerprint verification (dev/testing) |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | _(none)_ | Explicit path to TOML config file |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory |
| `GHOSTCAM_SERVER_ADDR` | _(from enrollment)_ | Server QUIC address |

Server address resolution precedence: `--server-addr` CLI flag -> `GHOSTCAM_SERVER_ADDR` env var -> config file -> `server.addr` file (written during enrollment) -> hardcoded default.

## How It Works

1. **Enrollment** — On first boot (no device cert), camera accepts an `--enrollment-jwt`. The server signs and returns a device certificate.
2. **TOFU** — On first connection after enrollment, the server's TLS fingerprint is pinned. Subsequent connections verify against the pin (bypassed with `--no-tofu`).
3. **Connect** — Opens a QUIC connection using the device cert for mTLS. Opens persistent streams: `Alerts` (bidirectional), `Video`, `Audio`.
4. **Handshake** — Sends an `Alert::Handshake` with device ID, cert fingerprint, and capabilities.
5. **Command loop** — Spawns a task reading `CameraCommand` messages from the server on the `Alerts` stream. Commands update `watch` channels that gate frame sending.
6. **Capture** — Starts capture modules producing frames on an mpsc channel.
7. **Recording** — Frames are also written to the local fMP4 ring buffer. Completed segments are announced via `Alert::RecordingSegment`; the server may request uploads.
8. **Send loop** — Reads from capture channel, checks command-controlled gates, sends frames on their persistent QUIC stream.
9. **Telemetry** — Independently polls system sensors every 2s, sends sparse diffs on a unidirectional upload stream.

## Module Map

| Module | Purpose |
|--------|---------|
| `main` | CLI, reconnect loop, top-level task orchestration |
| `config` | `CameraConfig` + `CameraConfigFile`, layered TOML/env/CLI resolution |
| `session` | Active QUIC session: alert stream, command stream, video/audio enabled atomics |
| `enrollment` | JWT parsing, enrollment handshake with server PKI |
| `tofu` | Server fingerprint pinning on first connect |
| `certs` | Device certificate load/store |
| `quic` | QUIC endpoint setup with mTLS |
| `commands` | `CameraCommand` handler — updates watch channels |
| `network` | Network interface monitoring (stub) |
| `firmware` | OTA update handling (stub) |
| `capture/mod` | `CaptureMessage` enum (VideoNal, AudioFrame) |
| `capture/video_test` | Test video source: loops H.264 file at real-time pace |
| `capture/audio_test` | Test audio source: synthetic Opus tone |
| `stream/mod` | Frame sender coordination |
| `stream/video` | Writes H.264 NAL units to the persistent Video QUIC stream |
| `stream/audio` | Writes Opus frames to the persistent Audio QUIC stream |
| `recording/mod` | Ring buffer coordinator |
| `recording/muxer` | fMP4 muxer (init segment + media segments) |
| `recording/ring_buffer` | Segment ring buffer with eviction |
| `recording/segment` | Segment state machine |
| `recording/manifest` | HLS playlist generation |
| `recording/manifest_push` | Pushes manifest to server via QUIC upload stream |
| `recording/uploads` | Uploads segments to server on demand |
| `recording/storage` | Segment persistence on disk |
| `recording/init` | fMP4 init segment generation |
| `recording/recovery` | Recover ring buffer state after crash |
| `telemetry/mod` | Telemetry task: poll → diff → encode → send |
| `telemetry/sensors` | Platform readers (`/proc/stat`, `/sys/class/thermal`, gpsd, etc.) |
| `telemetry/buffer` | Buffered telemetry for upload after reconnect |
