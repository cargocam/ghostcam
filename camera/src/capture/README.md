# camera/src/capture
Capture modules produce media frames for both live streaming and local recording.

## Public contract
`capture/mod.rs` defines:
- `CaptureMessage::VideoNal(Bytes)`
- `CaptureMessage::AudioFrame(Bytes)`
- `start_capture(config, cancel) -> CaptureReceiver`

Downstream modules treat this as the canonical camera-side media bus.

## Implementations in this directory
### `video_test.rs`
- Reads a raw Annex-B H.264 file from disk.
- Splits into NAL units.
- Emits one NAL every ~33.3ms (~30fps), looping forever.
- Honors cancellation token.

### `audio_test/`
- Emits pre-encoded Opus tone frames every 20ms (50fps).
- Uses static frame data (`opus_tone_data.rs`) to avoid runtime encoding dependencies.

## Source selection behavior
`start_capture()` has two branch points:
- `config.test_source == true`: test video + test audio.
- `config.test_source == false`: currently logs a warning and still uses test sources (real capture pipeline is staged for later implementation).

`config.no_audio` disables the audio task in both branches.

## Backpressure and reliability notes
- Channel capacity is bounded (`mpsc::channel(256)` in caller path).
- Capture tasks stop when receiver side is dropped.
- Video/audio ordering is preserved per source channel, then split/fanned out by main session orchestration.

## Why test sources are used in rewrite right now
The rewrite branch prioritizes transport/protocol correctness (enrollment, command handling, recording, uploads, WebRTC egress). Test capture sources provide deterministic media while hardware-specific capture integration is still pending.
