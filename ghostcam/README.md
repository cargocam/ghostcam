# ghostcam

Shared library for the Ghostcam system. Contains wire protocol definitions, serialization types, configuration constants, and reusable modules used by both the server and camera crates.

## Modules

| Module | Purpose |
|--------|---------|
| `audit` | `AuditEvent`, `AuditEntry`, `AuditLogger`: HMAC-SHA256 signed audit trail with broadcast channel |
| `command` | `CameraCommand`, `CommandResponse`: bridge→camera control messages (stream control, configure, reassign, custom) |
| `config` | Default constants (ports, MTU) |
| `data_channel` | `DataChannelMessage` enum and types for WebRTC data channel JSON (incl. `SdpAnswer` for renegotiation) |
| `frame` | 13-byte frame header codec for QUIC media streams |
| `group` | Hierarchical colon-separated group identifiers (`usr-alice:perimeter:north`) |
| `h264` | Annex-B NAL parser (`parse_h264_file`) and streaming `NalParser` |
| `hello` | `DeviceHello` handshake message (device_id, group_id, capabilities) |
| `quic` | Shared QUIC/TLS helpers: cert generation, server/client config, hello send/recv, command send/recv |
| `router` | `GroupRouter`: camera registry, broadcast channels (frames + events), SPS/PPS cache, telemetry state, command channels, group reassign |
| `rtp` | H.264 NAL→RTP packetizer (Single NAL + FU-A), timestamp math |
| `stream` | `send_video_frame`, `send_audio_frame`, `send_telemetry_frame`, `OPUS_SILENCE` |
| `telemetry` | `TelemetryData`, `SparseTelemetry`, `GpsData`; diff/merge logic, MessagePack encode/decode |

## Wire Format

Every media frame sent over QUIC uses this header:

```
Offset  Size  Field
0       1     stream_type   0=video (H.264 NAL), 1=audio (Opus), 2=telemetry (MessagePack SparseTelemetry)
1       8     timestamp_us  u64 big-endian, microseconds
9       4     payload_len   u32 big-endian
13      var   payload
```

Key functions:
- `Frame::encode(&self) -> Bytes`
- `Frame::decode(buf: &[u8]) -> io::Result<Frame>`
- `Frame::decode_header(buf: &[u8]) -> io::Result<(StreamType, u64, u32)>`

## Group Hierarchy

Groups use colon-separated IDs. `GroupId` provides:
- `is_ancestor_of(other)` — `"a"` is ancestor of `"a:b:c"`
- `parent()` — `"a:b:c"` → `Some("a:b")` → `Some("a")` → `None`

## Tests

```bash
cargo test -p ghostcam
```

31 unit tests: frame encode/decode roundtrip (incl. telemetry), group ancestry and parent traversal, RTP packetization (single NAL, FU-A, timestamp conversion), H.264 NAL parsing, `NalParser` streaming, telemetry encode/decode/diff/merge, command roundtrip (stream control, configure, custom, reassign, response, unknown type), router reassign (move, nonexistent, preserves others, command_tx cleanup), audit events (serialization, HMAC validation, sequence numbering), camera events (register emits joined, unregister emits left).
