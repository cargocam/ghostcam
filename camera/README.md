# camera

Ghostcam camera agent — streams H.264 video and Opus audio over QUIC to the bridge server. Connects, performs a `DeviceHello` handshake, then continuously sends media frames on unidirectional QUIC streams.

Supports two modes:
- **Test source** (`--test-source`): Loops a pre-recorded H.264 file with Opus silence. No system dependencies beyond Rust.
- **Real capture** (default): Uses `rpicam-vid`/`libcamera-vid` for video, `cpal` + Opus for audio, and reads `/proc`/`/sys` for system telemetry. Requires Linux with a camera.

Automatically reconnects to the bridge with exponential backoff (1s → 30s cap).

## System Requirements (Real Capture)

- `rpicam-vid` or `libcamera-vid` in PATH (Raspberry Pi OS, or any libcamera-enabled Linux)
- `libopus-dev` (Linux) or `brew install opus` (macOS, for cross-compilation)
- Optional: `gpsd` running on `localhost:2947` for GPS data

## Usage

```bash
# Generate test video (one-time, for --test-source mode)
mkdir -p test-data
ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test-data/test.h264

# Test source mode (any platform)
cargo run -p camera -- --test-source --bridge-addr 127.0.0.1:4433 --device-id cam-01 --group-id default

# Real capture (Linux with camera)
cargo run -p camera -- --bridge-addr 127.0.0.1:4433 --device-id cam-01 --group-id default

# Real capture with GPS
cargo run -p camera -- --enable-gps --device-id cam-01 --group-id default

# Launch multiple test cameras
./camera/launch-cameras.sh 4 default
```

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--bridge-addr` | `127.0.0.1:4433` | Bridge QUIC address (host:port) |
| `--device-id` | `test-cam-01` | Unique device identifier |
| `--group-id` | `default` | Camera group assignment |
| `--test-source` | _(off)_ | Use test source instead of real capture |
| `--test-file` | `test-data/test.h264` | Raw H.264 Annex-B file (for `--test-source`) |
| `--width` | `1280` | Video width (real capture) |
| `--height` | `720` | Video height (real capture) |
| `--fps` | `30` | Target frame rate |
| `--bitrate` | `0` (auto) | Video bitrate in bits/s |
| `--keyframe-interval` | `60` | Keyframe interval in frames |
| `--no-audio` | _(off)_ | Disable audio capture |
| `--no-telemetry` | _(off)_ | Disable telemetry collection |
| `--enable-gps` | _(off)_ | Enable GPS via gpsd |

## How It Works

1. **Connect** — Generates self-signed cert, opens QUIC connection to bridge
2. **Handshake** — Sends `DeviceHello` JSON on bidirectional control stream
3. **Capture** — Starts capture modules (video, audio, telemetry) which produce `CaptureMessage`s on a channel
4. **Send loop** — Reads from capture channel, sends each message on a unidirectional QUIC stream with 13-byte frame header
5. **Reconnect** — On connection loss, drains capture messages during backoff, then reconnects

### Test Source Mode

- Video: Parses H.264 NALs from test file, loops indefinitely with FPS-paced timing
- Audio: Sends 3-byte Opus silence frames every 20ms
- No telemetry (server shows no telemetry in viewer, fine for dev)

### Real Capture Mode

- **Video**: Spawns `rpicam-vid` (or `libcamera-vid`) as subprocess, reads H.264 from stdout, parses NAL units via streaming `NalParser`
- **Audio**: `cpal` default input → stereo-to-mono → resample to 48kHz → Opus encode (20ms frames). Non-fatal if no audio device.
- **Telemetry**: Polls `/proc/stat` (CPU), `/sys/class/thermal` (temp), `/proc/meminfo` (memory), `/proc/uptime`, `/proc/loadavg`, `/proc/net/dev` (network). GPS via gpsd TCP. Sends sparse diffs with thresholds, full heartbeat every 30s. MessagePack-encoded as `StreamType::Telemetry`.

## Modules

| Module | Purpose |
|--------|---------|
| `main` | CLI parsing, capture orchestration, QUIC reconnect loop |
| `quic` | QUIC client setup (uses `ghostcam::quic` helpers) |
| `capture/mod` | `CaptureMessage` enum (VideoNal, Audio, Telemetry) |
| `capture/video` | `rpicam-vid` subprocess + streaming NAL parser |
| `capture/audio` | `cpal` input + Opus encoding |
| `capture/telemetry` | Linux `/proc`/`/sys` readers + gpsd client |

Shared modules in `ghostcam` lib: `h264` (NAL parser, `NalParser`), `stream` (frame send), `quic` (cert gen, hello), `telemetry` (SparseTelemetry types).

## Notes

- Server certificate verification is disabled (dev only). Production will use mTLS.
- The agent is currently write-only — the bridge does not send commands back over QUIC.
- `rpicam-vid` child process is killed on drop (`kill_on_drop(true)`) and on SIGTERM/SIGINT.
- Audio capture failure is non-fatal — the camera continues without audio.
- Telemetry readers return zero/None on non-Linux platforms (stubs for cross-compilation).
