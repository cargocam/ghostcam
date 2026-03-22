# camera
`camera` is the device-side runtime. It owns enrollment, QUIC session management, media capture fanout, local fMP4 recording, command execution, and telemetry buffering.

This README describes the runtime at the current `rewrite` branch shape.

## What this process does
At startup the camera binary:
1. Resolves configuration (`CLI` → `/boot/ghostcam.conf` → `<data_dir>/server.addr` fallback).
2. Loads or creates persistent device identity (`device.crt` + `device.key`).
3. Optionally runs enrollment if no user association cert exists (`user.crt` + `user.key`).
4. Starts capture sources and a telemetry loop.
5. Connects to the server over QUIC and establishes a control/media session.
6. Runs until shutdown, reconnecting with exponential backoff on disconnect.

## Runtime layout
- `src/main.rs`: process orchestration and reconnect loop.
- `src/session.rs`: one live QUIC session, command reader, stream writers, recording/upload workers.
- `src/commands.rs`: server command handling (`start/stop`, uploads, network config, firmware, unregister).
- `src/enrollment.rs`: JWT-driven enrollment flow (token + CSR + signed cert install).
- `src/certs.rs`: cert/key load or generation helpers.
- `src/quic.rs`: client endpoint + connect helpers.
- `src/capture/`: capture sources (`video_test`, `audio_test`) and `CaptureMessage` definitions.
- `src/recording/`: local segmenter/muxer, ring buffer, manifest generation, upload responders.
- `src/telemetry/`: sensor reads + buffered datagram sender.
- `src/network.rs`: `nmcli`-based Wi-Fi command handlers.
- `src/firmware.rs`: update download/verify/swap/exit flow.

Per-component docs:
- `src/README.md`
- `src/capture/README.md`
- `src/stream/README.md`
- `src/recording/README.md`
- `src/telemetry/README.md`

## Data directory contract
Default data root is `/var/ghostcam` (override with `--data-dir`).

Important files:
- `device.crt`, `device.key`: long-lived device identity.
- `user.crt`, `user.key`: enrollment-issued user association cert/key.
- `ca.crt`: server CA (optional, received during cert refresh / enrollment).
- `server.addr`: remembered QUIC endpoint.
- `server_fingerprint`: TOFU pin written during enrollment.
- `telemetry.buf`: persisted telemetry backlog while offline.
- `firmware/current`, `firmware/previous`, `firmware/healthy`: update + watchdog markers.
- `segments/*.m4s` + generated init/manifest state via recording pipeline.

## CLI flags
Implemented flags in `main.rs`:
- `--server-addr <host:port>`
- `--test-source`
- `--test-video <path>`
- `--segment-dir <path>`
- `--no-audio`
- `--no-gps`
- `--data-dir <path>`
- `--enrollment-jwt <jwt>`
- `--no-tofu`

Notes:
- `--segment-dir` is parsed into config, but the active session path currently derives segments from `<data_dir>/segments`.
- `--no-tofu` is currently parsed but not applied in the connect path.

## Enrollment and identity model
Enrollment path (`src/enrollment.rs`) is used when no `user.crt` exists:
1. Parse enrollment JWT (without local signature validation).
2. Connect with device cert only.
3. Open control bidi stream and send `Alert::Enrollment { token }`.
4. Send CSR (`Alert::Csr`).
5. Receive `Command::CertRefresh`.
6. Ack `cert_refresh`, then persist new cert chain artifacts.

After enrollment, reconnects include the user cert as the second certificate in the client chain.

## Session model (camera ↔ server)
For each connection (`Session::establish` / `Session::run`):
- Open control stream and send `InboundStreamTag::Alerts` + `Alert::Handshake`.
- Open persistent unidirectional media streams:
  - `InboundStreamTag::Video`
  - `InboundStreamTag::Audio`
- Spawn concurrent workers:
  - command reader
  - video writer
  - audio writer
  - recording muxer
  - segment event handler
  - upload command handler
- Upload any buffered telemetry backlog once at session establishment.

Server commands toggle atomic `video_enabled` / `audio_enabled` flags; local recording still continues even if live streaming is disabled.

## Recording behavior
The recording pipeline (`src/recording`) writes rolling fMP4 segments and emits events:
- `SegmentEvent::InitReady`
- `SegmentEvent::Finalized`
- `SegmentEvent::ManifestUpdated`
- storage / eviction events

Upload commands from server (`UploadSegment`, `UploadInit`) are fulfilled from this local store.

## Telemetry behavior
Telemetry loop (`src/telemetry/mod.rs`):
- polls sensors every `TELEMETRY_POLL_INTERVAL_SECS`,
- sends on threshold changes or heartbeat interval,
- writes to QUIC datagrams when connected,
- otherwise appends to `TelemetryBuffer` with run-length deduplication,
- flushes to disk on shutdown.

## Current implementation notes
- “Real capture” mode currently falls back to test audio/video sources (capture hardware pipeline is stubbed for future plans).
- Stream writer helpers in `src/stream/` are maintained, but session currently inlines equivalent writer loops.
- TOFU verification helpers exist in `src/tofu.rs`, but main session connect path does not currently invoke them.
