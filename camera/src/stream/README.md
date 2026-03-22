# camera/src/stream
This directory contains reusable stream writer loops for camera → server media transport.

## Files
- `video.rs`: `run_video_writer(...)`
- `audio.rs`: `run_audio_writer(...)`

## Responsibilities
Each writer:
- reads `CaptureMessage` from an `mpsc::Receiver`,
- checks an atomic enable flag (`video_enabled` / `audio_enabled`),
- writes length-prefixed frames to a QUIC send stream using `ghostcam::wire::framing::write_frame`,
- exits cleanly on cancellation or channel close.

## Current runtime usage
The active session path in `session.rs` inlines equivalent writer logic directly inside spawned tasks. These helpers remain useful as:
- reusable unit-testable loops,
- reference implementations for future refactors,
- a clear boundary for transport-only media forwarding behavior.

## Behavioral guarantees
- Messages for other media types are ignored (non-matching enum variants are skipped).
- Disabled state drains input without writing, so producers do not block.
- Framing errors are surfaced as `anyhow::Result` failures.
