# Real Video Capture — Design Document

## Overview

Replace the test-source H.264 loop with real camera capture via `rpicam-vid` (Raspberry Pi OS 12+), with `libcamera-vid` as a fallback for older OS versions. Video is piped as raw H.264 Annex B to stdout and read by the camera process.

## Hardware

- Raspberry Pi 4/5 or Zero 2W
- IMX219 (Pi Camera Module v2) or compatible CSI camera
- Hardware H.264 encoding via the Pi's GPU

## Architecture

```
rpicam-vid (subprocess)
    │ stdout: raw H.264 Annex B
    ▼
spawn_blocking reader
    │ Bytes chunks
    ▼
NAL parser (existing ghostcam::wire::frames)
    │ parsed NAL units
    ▼
mpsc::Sender<CaptureMessage::Video>
    │
    ├──► QUIC video stream (to server)
    └──► fMP4 muxer (local recording)
```

## Implementation

### Binary Detection

On startup, check which capture binary is available:

```rust
async fn detect_capture_binary() -> Option<&'static str> {
    for bin in ["rpicam-vid", "libcamera-vid"] {
        if tokio::process::Command::new("which")
            .arg(bin)
            .output()
            .await
            .map(|o| o.status.success())
            .unwrap_or(false)
        {
            return Some(bin);
        }
    }
    None
}
```

If neither is found and `--test-source` is not set, the camera exits with an error (not a silent fallback).

### Subprocess Arguments

```
rpicam-vid \
  -t 0                    # run indefinitely
  --width 1280            # configurable
  --height 720            # configurable
  --framerate 30          # configurable
  --codec h264            # H.264 output
  --profile baseline      # broadest decoder compatibility
  -o -                    # stdout
  --flush                 # immediate output (no buffering)
  -n                      # no preview window (headless)
  --inline                # SPS/PPS with every keyframe
  --bitrate 2000000       # 2 Mbps default, ABR-controlled
  --intra 60              # keyframe every 2s at 30fps
```

### Reader Task

The subprocess stdout is read in a `spawn_blocking` task to avoid blocking the async runtime:

```rust
let child = Command::new(binary)
    .args(&args)
    .stdout(Stdio::piped())
    .stderr(Stdio::piped())
    .spawn()?;

let stdout = child.stdout.take().unwrap();
tokio::task::spawn_blocking(move || {
    let mut buf = vec![0u8; 32768]; // 32KB read buffer
    loop {
        let n = stdout.read(&mut buf)?;
        if n == 0 { break; } // process exited
        tx.blocking_send(CaptureMessage::Video(Bytes::copy_from_slice(&buf[..n])))?;
    }
});
```

The existing NAL parser downstream handles splitting Annex B byte streams into individual NAL units.

### Configuration

New fields in `CameraConfig` / `CameraConfigFile`:

```rust
/// Video resolution width (default: 1280)
pub video_width: u32,
/// Video resolution height (default: 720)
pub video_height: u32,
/// Video framerate (default: 30)
pub video_fps: u32,
/// Video bitrate in bps (default: 2_000_000). 0 = VBR
pub video_bitrate: u32,
/// Keyframe interval in frames (default: 60 = 2s at 30fps)
pub video_keyframe_interval: u32,
```

Environment variables: `GHOSTCAM_VIDEO_WIDTH`, `GHOSTCAM_VIDEO_HEIGHT`, `GHOSTCAM_VIDEO_FPS`, `GHOSTCAM_VIDEO_BITRATE`, `GHOSTCAM_VIDEO_KEYFRAME_INTERVAL`.

### Preset Profiles

For convenience and ABR:

| Profile | Resolution | Bitrate | Use Case |
|---------|-----------|---------|----------|
| minimum | 854x480 | 500 Kbps | Cellular fallback |
| low | 854x480 | 1 Mbps | Low bandwidth |
| medium | 1280x720 | 2 Mbps | Default |
| high | 1920x1080 | 4 Mbps | LAN / good WiFi |

### Bitrate Changes (ABR) — Deferred

**Status: Not yet implemented.** ABR is planned for a future iteration.

When implemented, the ABR controller will request tier changes by restarting `rpicam-vid` with new `--bitrate` and potentially `--width`/`--height` arguments (same approach as Kodama):

1. Kill existing subprocess
2. Spawn new subprocess with updated args
3. Wait for first keyframe before forwarding frames (prevents decoder glitches)
4. `--inline` ensures SPS/PPS is included with the first keyframe

The restart takes ~200-500ms on Pi hardware. During this gap, the server receives no frames — viewers see a brief freeze, which is acceptable for a bitrate change.

For v1, the camera uses a fixed bitrate set at startup via config.

### Process Lifecycle

- **Start**: `VideoCapture::start()` spawns subprocess + reader task, returns `mpsc::Receiver`
- **Stop**: `VideoCapture::stop()` sends SIGTERM to subprocess, waits with timeout, then SIGKILL
- **Drop**: `impl Drop` ensures cleanup if the capture handle is dropped
- **Crash recovery**: If `rpicam-vid` exits unexpectedly, the reader task detects EOF and signals the capture manager, which can restart it

### Error Handling

- `rpicam-vid` not found → exit with clear error message
- Camera device busy → retry with backoff (another process may hold `/dev/video0`)
- Subprocess crash → log error, restart after 1s delay
- Reader channel full → newest frames dropped (backpressure from slow QUIC)

## Test Source Preservation

The existing test source (`--test-source`) remains for development and CI. The capture module dispatches based on config:

```rust
if config.test_source {
    start_test_capture(...)    // existing loop
} else {
    start_real_capture(...)    // new rpicam-vid pipeline
}
```

No behavioral changes to test mode.

## Dependencies

No new Rust crate dependencies. `rpicam-vid` is a system binary (installed via `rpicam-apps` package on Pi OS).
