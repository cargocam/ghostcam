# Real Audio Capture — Design Document

## Overview

Replace the synthetic Opus tone loop with real microphone capture using `cpal` for audio input and the `opus` crate for encoding. Output is 20ms Opus frames matching the existing audio pipeline.

## Hardware

- USB microphone or Pi-compatible I2S/ALSA input device
- SIM7600G-H modem (has built-in microphone on some variants — not used, dedicated mic preferred)

## Architecture

```
Microphone (ALSA)
    │
cpal input stream (std::thread — cpal is !Send)
    │ raw PCM samples (f32 or i16, mono or stereo)
    ▼
Format conversion + resampling
    │ 48kHz mono f32
    ▼
Opus encoder (20ms frames = 960 samples)
    │ encoded Opus bytes
    ▼
mpsc::Sender<CaptureMessage::Audio>
    │
    ├──► QUIC audio stream (to server)
    └──► fMP4 muxer (local recording)
```

## Implementation

### Threading Model

`cpal` audio streams are `!Send` — they cannot be used across tokio tasks. Following Kodama's pattern:

1. Audio capture runs on a dedicated `std::thread` (not a tokio task)
2. The cpal stream callback sends raw samples via `std::sync::mpsc`
3. A processing thread accumulates samples into 20ms buffers, encodes with Opus, and sends via `tokio::sync::mpsc`

```rust
std::thread::Builder::new()
    .name("audio-capture".into())
    .spawn(move || {
        // cpal stream setup + encoding loop
    })?;
```

### Device Selection

```rust
let host = cpal::default_host();
let device = if let Some(name) = &config.audio_device {
    host.input_devices()?
        .find(|d| d.name().map(|n| n == *name).unwrap_or(false))
        .context("audio device not found")?
} else {
    host.default_input_device()
        .context("no default audio input device")?
};
```

### Format Negotiation

Query the device's supported configs and pick the closest match to our target (48kHz mono):

- **Target**: 48000 Hz, 1 channel, f32 or i16
- **Fallback**: Accept stereo (mix to mono), accept different sample rates (resample)

### Audio Processing Pipeline

1. **Format conversion**: cpal may deliver f32, i16, or i32. Normalize to f32 in range [-1.0, 1.0]
2. **Channel mixing**: If stereo, average L+R to mono: `(left + right) / 2.0`
3. **Resampling**: If device sample rate != 48000, linear interpolation resample (same as Kodama)
4. **Buffering**: Accumulate exactly 960 samples (20ms at 48kHz) per Opus frame
5. **Opus encoding**: Encode 960 f32 samples → compressed Opus bytes
6. **Send**: Forward encoded frame via channel

### Opus Encoder Setup

```rust
let encoder = opus::Encoder::new(48000, opus::Channels::Mono, opus::Application::Voip)?;
// VoIP mode optimizes for speech (surveillance use case)

let mut pcm_buf = Vec::with_capacity(960);
let mut opus_buf = vec![0u8; 4000]; // max Opus frame size

loop {
    // Accumulate 960 samples from cpal callback channel
    while pcm_buf.len() < 960 {
        let samples = raw_rx.recv()?;
        pcm_buf.extend(process_samples(&samples));
    }

    let encoded_len = encoder.encode_float(&pcm_buf[..960], &mut opus_buf)?;
    pcm_buf.drain(..960);

    tx.blocking_send(CaptureMessage::Audio(
        Bytes::copy_from_slice(&opus_buf[..encoded_len])
    ))?;
}
```

### Configuration

New fields in `CameraConfig`:

```rust
/// Audio input device name (default: system default)
pub audio_device: Option<String>,
```

Environment variable: `GHOSTCAM_AUDIO_DEVICE`.

Existing `--no-audio` / `no_audio` flag continues to work — skips audio capture entirely.

### Graceful Degradation

If audio capture fails to initialize (no microphone, device busy, ALSA error):
- Log a warning
- Continue without audio — video-only operation
- Do not crash the camera process

This matches the existing behavior where `--no-audio` disables audio without affecting video.

### Error Recovery

- Device disconnected mid-stream → cpal error callback fires → log warning, attempt to reopen device after 5s
- Opus encode error → skip frame (extremely rare with valid PCM input)
- Channel full → drop frame (backpressure)

## Dependencies

New crate dependencies for `camera/Cargo.toml`:

```toml
[target.'cfg(target_os = "linux")'.dependencies]
cpal = "0.15"
opus = "0.3"
```

System packages needed on Pi: `libasound2-dev` (build), `libasound2` (runtime), `libopus-dev` (build), `libopus0` (runtime). These are already in the Dockerfile.

## Test Source Preservation

The existing synthetic Opus tone source (`capture/audio_test/`) remains for `--test-source` mode. No changes to test audio.
