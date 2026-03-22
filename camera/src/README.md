# camera/src
This directory is the camera runtime implementation. It composes identity management, QUIC transport, control protocol handling, recording, telemetry, and command-side effects.

## Top-level module map
- `main.rs`: process entrypoint and reconnect orchestration.
- `config.rs`: config source precedence and parsing.
- `certs.rs`: local cert/key load/create helpers.
- `enrollment.rs`: enrollment handshake + disk persistence.
- `quic.rs`: client endpoint and connect helpers.
- `session.rs`: one active session lifecycle and worker fanout.
- `commands.rs`: server command decoding and dispatch.
- `network.rs`: Wi-Fi command implementation (`nmcli`).
- `firmware.rs`: download/verify/install restart flow.
- `tofu.rs`: server-fingerprint verification helpers.
- `capture/`: media sources.
- `stream/`: reusable stream writer loops.
- `recording/`: local segment generation + upload handling.
- `telemetry/`: sensor polling + buffering.

## Runtime message flow
1. `capture::start_capture()` emits `CaptureMessage` (`VideoNal` / `AudioFrame`).
2. `main.rs` fans those into separate persistent channels.
3. `session.rs` bridges persistent channels into per-session channels.
4. Session writers forward live media to tagged QUIC streams when enabled by commands.
5. The muxer also consumes those frames for local recording regardless of live state.

## Session worker set (`session.rs`)
During `Session::run`, the process runs:
- command reader (`commands_rx` JSON framing),
- video writer task,
- audio writer task,
- recording muxer task,
- segment event handler task (manifest + alerts),
- upload handler task (UploadInit / UploadSegment fulfillment).

Cancellation and command-triggered exits collapse the session, after which reconnect logic in `main.rs` attempts recovery.

## Wire primitives used here
The camera side imports shared protocol types from `ghostcam` and `server-core`:
- Alerts / commands: `ghostcam::wire::alert::Alert`, `ghostcam::wire::command::Command`
- Framing: `ghostcam::wire::framing::{read_json, write_json, read_frame, write_frame}`
- Stream tags: `server_core::frames::InboundStreamTag`

## Notes on currently staged modules
- `tofu.rs` exports verification/storage helpers for server fingerprint pinning; current connect path does not invoke them yet.
- `stream/` exposes writer helpers that mirror current inline session writer logic.
- `recording/storage.rs` and `recording/recovery.rs` are present for storage pressure and crash recovery flows; the active session currently centers on muxer + ring buffer + upload responders.
