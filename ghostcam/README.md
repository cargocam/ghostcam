# ghostcam (shared library)

Shared types and protocol definitions used by both the `camera` and `server` crates. No I/O, no heavy async dependencies — just types, constants, and serialization.

## Modules

### `config`

Compile-time constants shared across crates:

| Constant | Value | Purpose |
|----------|-------|---------|
| `QUIC_PORT` | 4433 | Camera → server QUIC ingest port |
| `HTTP_PORT` | 3000 | Server HTTP API port |
| `BROADCAST_CAPACITY` | 2048 | Video/audio broadcast channel buffer size |
| `MAX_FRAME_SIZE` | 4 MB | Maximum inbound QUIC frame payload |
| `TELEMETRY_POLL_INTERVAL` | 2s | Sensor read cadence on camera |
| `TELEMETRY_FULL_INTERVAL` | 30s | Full heartbeat cadence on camera |

### `types`

Newtype wrappers with `serde`, `Display`, and `From<String>`:

| Type | Underlying | Purpose |
|------|-----------|---------|
| `DeviceId` | `String` | Unique camera identifier |
| `UserId` | `String` | Operator/owner identifier |
| `SessionId` | `String` | WebRTC session identifier |
| `TokenId` | `String` | API token identifier |
| `CertFingerprint` | `String` | Hex-encoded SHA-256 cert fingerprint |
| `Seq` | `u64` | Monotonic sequence number |

### `telemetry`

`TelemetryDatagram` — sparse telemetry payload sent from camera over QUIC. All fields are `Option` — only changed values are included per send. A full heartbeat (all fields) is forced every 30 seconds.

Fields: `sig` · `temp` · `fps` · `kbps` · `cpu` · `mem` · `uptime` · `lat` · `lon` · `alt` · `gps_fix`

Serialized as MessagePack (`rmp-serde`) for compact wire encoding. Diff thresholds gate what counts as "changed": CPU 5%, temperature 1°C, memory 5 MB, GPS 0.0001°.

### `pki`

Thin wrappers around `rcgen` 0.13 for certificate generation:

- `generate_key_pair()` — ECDSA P-256 key pair
- `create_self_signed_ca(key_pair, subject)` — self-signed CA certificate

Used during server bootstrap and camera enrollment.

### `wire`

Wire protocol types for camera ↔ server communication over QUIC. See [`src/wire/README.md`](src/wire/README.md).

## Tests

```bash
cargo test -p ghostcam
```
