# camera/src/telemetry
Telemetry modules collect device state, decide when to emit updates, and persist data when disconnected.

## Module split
- `mod.rs`: run loop and send/buffer decision logic.
- `sensors.rs`: platform-specific telemetry readers and GPS hooks.
- `buffer.rs`: in-memory + on-disk buffer with deduplication.

## Run loop (`run_telemetry_loop`)
Inputs:
- `watch::Receiver<Option<quinn::Connection>>` for live connection state.
- `TelemetryBuffer` instance.
- `CameraConfig` (notably `no_gps`).

Behavior:
1. Poll sensors every `TELEMETRY_POLL_INTERVAL_SECS`.
2. Emit only when thresholds are exceeded or heartbeat interval elapsed.
3. If connected, send MessagePack datagram over QUIC datagrams.
4. If disconnected (or send fails), append to `TelemetryBuffer`.
5. Flush buffer to disk on cancellation.

Threshold logic comes from shared `ghostcam::telemetry::exceeds_threshold`.

## Sensor model (`sensors.rs`)
`TelemetryDatagram` fields include:
- CPU, memory, temperature, uptime, Wi-Fi signal
- optional GPS (`lat`, `lon`, `alt`, `gps_fix`)
- optional stream metrics (`fps`, `kbps`) reserved for producer-side feed stats

Platform behavior:
- Linux: `/proc` + `/sys` readers.
- Non-Linux: synthetic development defaults.
- GPS:
  - disabled when `no_gps` is set,
  - Linux path currently returns `None` placeholders,
  - non-Linux path generates synthetic moving coordinates for UI development.

## Buffer behavior (`TelemetryBuffer`)
Buffer file default is `<data_dir>/telemetry.buf`.

Features:
- bounded capacity (`TELEMETRY_BUFFER_CAP`)
- run-length deduplication for repeated identical samples (preserves first + latest timestamp in a run)
- batch encode/decode via shared `TelemetryDatagram::{encode_batch, decode_batch}`
- explicit `drain()` + `clear_disk()` for upload completion

On session establishment, the camera uploads drained buffered telemetry through a one-shot `InboundStreamTag::TelemetryBuffer` stream.
