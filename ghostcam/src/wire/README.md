# wire

Wire protocol types for camera ↔ server communication over QUIC. All types in this module are shared — both camera and server import them from `ghostcam::wire`.

## Modules

### `framing`

Async length-prefixed frame I/O over QUIC streams. All QUIC control messages use this framing:

```
[4 bytes: payload length (u32 BE)] [payload bytes]
```

Functions:
- `write_frame(stream, bytes)` — write a length-prefixed frame
- `read_frame(stream, max_size)` — read one frame, reject oversized payloads
- `write_json<T>(stream, value)` — serialize to JSON then write as framed bytes
- `read_json<T>(stream, max_size)` — read a frame then deserialize from JSON

Used by the alerts channel, command channel, and enrollment flow.

### `frames`

QUIC stream type tags and frame types for inbound unidirectional streams.

**`InboundStreamTag`** — first byte written by the camera on each unidirectional stream. Identifies the stream's purpose:

| Tag | Value | Stream type |
|-----|-------|-------------|
| `Segment` | `0x00` | Upload: fMP4 media segment |
| `Init` | `0x01` | Upload: fMP4 init segment |
| `Manifest` | `0x02` | Upload: HLS manifest |
| `TelemetryBuffer` | `0x03` | Upload: buffered telemetry array |
| `Alerts` | `0x10` | Persistent: alerts / handshake channel |
| `Video` | `0x11` | Persistent: length-prefixed H.264 NAL units |
| `Audio` | `0x12` | Persistent: length-prefixed Opus frames |

Upload streams are one-shot (camera opens, sends data, closes). Persistent streams stay open for the connection lifetime.

**`VideoFrame`** / **`AudioFrame`** — typed wrappers around `Bytes` for broadcast channels within the server's ingest pipeline.

### `command`

`CameraCommand` — server → camera control messages, sent on the command QUIC stream using `framing::write_json`. Tagged enum (`type` field in JSON):

| Variant | Fields | Purpose |
|---------|--------|---------|
| `StartVideo` | — | Resume video streaming |
| `StopVideo` | — | Pause video streaming |
| `StartAudio` | — | Resume audio streaming |
| `StopAudio` | — | Pause audio streaming |
| `UploadSegment` | `segment_id` | Request camera upload a recorded segment |
| `UploadInit` | — | Request camera upload the fMP4 init segment |
| `Reboot` | — | Reboot the camera device |
| `NetworkConfig` | `ssid`, `psk` | Configure WiFi |
| `RemoveNetwork` | `ssid` | Remove a WiFi network |
| `ListNetworks` | — | Request network list |
| `UpdateAvailable` | `version`, `url`, `checksum` | Notify camera of a firmware update |
| `CertRefresh` | `cert_pem` | Push a new client certificate |
| `Unregister` | — | Revoke enrollment and wipe camera state |

All commands carry a `seq: Seq` field for correlation with `Ack` alerts.

### `alert`

`Alert` — camera → server messages, sent on the persistent `Alerts` QUIC stream using `framing::write_json`. Tagged enum:

| Variant | Fields | Purpose |
|---------|--------|---------|
| `Handshake` | `device_id`, `cert_fingerprint`, `capabilities` | First message after connect; identifies the camera |
| `CapabilityUpdate` | `capabilities` | Notify server of changed capabilities |
| `RecordingSegment` | `segment_id`, `duration_ms`, `size_bytes`, `start_ts` | New segment available in ring buffer |
| `SegmentEvicted` | `segment_id` | Segment dropped from ring buffer |
| `SegmentUploaded` | `segment_id` | Confirmed upload to server |
| `SegmentUploadFailed` | `segment_id`, `reason` | Upload failed |
| `Ack` | `seq` | Acknowledge receipt of a `CameraCommand` |
| `Enrollment` | `enrollment_token` | Camera presenting an enrollment JWT |
