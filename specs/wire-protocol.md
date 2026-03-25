# Ghostcam — Camera/Server Wire Protocol

**Status:** Draft

---

## 1. Overview

This document specifies the wire format for all data exchanged between a Ghostcam camera and the Ghostcam server. It covers the QUIC channel layout, channel payload formats, stream framing, and message schemas for each channel.

The camera runs on a Raspberry Pi Zero 2W. The server is a Rust service using Quinn (QUIC) for camera ingest and str0m (WebRTC) for observer egress. All camera-to-server connections use mutual TLS.

Live video and audio stream over the QUIC Video and Audio channels to the server, which fans them out to observers over WebRTC. The QUIC video and audio streams carry live data exclusively — historic footage retrieval is specified in `playback.md`. Telemetry is sent live and may also be buffered on the camera for upload on reconnect; historic telemetry queries are specified in `telemetry.md`. Authentication and certificate lifecycle are specified in `auth.md`.

Protocol version is declared once per connection in the `handshake` message (see §6.1). There is no per-frame header in v1 — payloads are written directly to their respective channels with no framing prefix beyond the length delimiter on stream channels.

---

## 2. QUIC Channel Layout

The camera-to-server connection uses the following QUIC channels, each chosen to match the delivery semantics of its data type.

| Channel | Transport | Direction | Rationale |
|---------|-----------|-----------|-----------|
| Commands | QUIC unidirectional stream | Server → Camera | Reliable, ordered; server only ever sends commands |
| Alerts | QUIC unidirectional stream | Camera → Server | Reliable, ordered; camera only ever sends alerts |
| Video | QUIC unidirectional stream | Camera → Server | Per-stream HOL isolation from audio; reliable delivery required for H.264 |
| Audio | QUIC unidirectional stream | Camera → Server | Per-stream HOL isolation from video; reliable delivery required for Opus |
| Telemetry | QUIC datagrams | Camera → Server | Latest-value semantics; loss-tolerant; high-frequency sensor readings |
| Upload streams | QUIC unidirectional streams (on demand) | Camera → Server | Segment, init, manifest, and telemetry buffer uploads; camera-initiated |

Video and audio streams are opened at connect time regardless of whether capture is currently active. If a stream is inactive, no bytes are written — the stream remains open but idle.

Upload streams are opened on demand — one per upload. See `playback.md` for segment and manifest upload streams, and §5.2 for the telemetry buffer upload stream.

---

## 3. Channel Payload Formats

Each channel carries a single payload format. The server and camera dispatch on channel identity, not on any frame-level type field.

| Channel | Payload format |
|---------|---------------|
| Video | Raw H.264 NAL unit(s), length-prefixed per frame (see §4) |
| Audio | Raw Opus packet, length-prefixed per frame (see §4) |
| Telemetry | MessagePack-encoded map, self-contained per datagram (see §5) |
| Alerts | JSON-encoded message, length-prefixed (see §4, §6) |
| Commands | JSON-encoded message, length-prefixed (see §4, §7) |
| Upload streams | Raw bytes; format depends on upload type (see `playback.md`, §5.2) |

---

## 4. Stream Framing

QUIC streams are byte-oriented. All stream channels (Video, Audio, Alerts, Commands) delimit messages using a 4-byte big-endian length prefix:

```
 ┌──────────┬──────────────────────────────────────┐
 │  length  │            frame data                │
 │ 4 bytes  │          (length bytes)              │
 └──────────┴──────────────────────────────────────┘
```

- `length` is a `u32` big-endian value indicating the byte count of `frame data` only, excluding the length field itself.
- The receiver reads the 4-byte length, then exactly `length` bytes.
- Maximum frame size is 4 MB (`0x00400000`). Frames exceeding this limit MUST be rejected and the stream MUST be reset.

Telemetry uses QUIC datagrams — each datagram is a self-contained message requiring no length framing.

Upload streams carry raw bytes with no length framing — stream close signals end of data.

---

## 5. Telemetry

### 5.1 Live telemetry datagrams (MessagePack)

Telemetry datagrams carry sensor readings from the camera. The camera polls sensors every 2 seconds and transmits immediately when any field exceeds its per-field threshold. A full heartbeat is transmitted every 30 seconds regardless of threshold state. When the QUIC connection is available, each datagram is sent directly to the server. When offline, datagrams are written to the on-disk buffer instead (see §5.2). All fields are optional; the camera includes only fields for which valid readings are currently available.

Historic telemetry queries are handled via REST and are specified in `telemetry.md`.

**Schema**

| Field | Type | Description |
|-------|------|-------------|
| `ts` | `u64` | Unix timestamp in milliseconds (camera clock) |
| `sig` | `i8` | WiFi signal strength (dBm) |
| `temp` | `u32` | SoC temperature (°C) |
| `fps` | `f32` | Current video capture frame rate |
| `kbps` | `u32` | Current video bitrate (kbps) |
| `cpu` | `u32` | CPU usage (%) |
| `mem` | `u32` | Memory usage (MB) |
| `uptime` | `u32` | System uptime (seconds) |
| `lat` | `f64` | GPS latitude (decimal degrees). Omitted if GPS hardware is absent or has no fix. |
| `lon` | `f64` | GPS longitude (decimal degrees). Omitted if GPS hardware is absent or has no fix. |
| `alt` | `f32` | GPS altitude (metres). Omitted if GPS hardware is absent or has no fix. |
| `gps_fix` | `u8` | GPS fix quality: `0` = none, `1` = 2D, `2` = 3D. Omitted if GPS hardware is absent. |

GPS hardware is optional. When the `gpsd` socket is unavailable the camera omits all GPS fields silently — this is not an error condition.

**Example (JSON representation)**

```json
{
  "ts": 1700000000123,
  "sig": -62,
  "temp": 54,
  "fps": 29.97,
  "kbps": 2400,
  "cpu": 23,
  "mem": 312,
  "uptime": 86400,
  "lat": 37.7749,
  "lon": -122.4194,
  "alt": 15.2,
  "gps_fix": 2
}
```

### 5.2 Telemetry buffer upload stream

When the QUIC connection is unavailable, the camera writes telemetry datagrams to an on-disk buffer rather than dropping them. The buffer stores standard telemetry datagrams — the same format as live datagrams — and applies deduplication to avoid accumulating redundant heartbeat entries. The buffer is capped at 100,000 entries; oldest entries are evicted when full. See `camera-firmware.md` §7.3 for the deduplication algorithm and cap behaviour.

On reconnect, after the handshake and manifest push, if the on-disk buffer contains entries the camera opens a new outbound QUIC unidirectional stream and writes all buffered entries as a MessagePack array. Each entry in the array is a standard telemetry datagram map (§5.1 schema). The camera closes the stream when all entries have been written and clears the buffer after the stream closes successfully.

The server reads the full stream, decodes the MessagePack array, and persists each entry to Redis using the same write path as live datagrams. No special handling is required for buffered vs. live entries.

No command or alert is required — the telemetry buffer upload is fully camera-initiated.

---

## 6. Alerts Channel — Camera → Server (JSON)

All alert messages share a `type` discriminant field. The server dispatches on `type`.

### 6.1 `handshake`

Sent as the first message on every new QUIC connection. The server reconstructs all slot state from this message. Carries the protocol version so the server can detect wire format incompatibilities at connection time.

```json
{
  "type": "handshake",
  "protocol_version": 1,
  "fw_version": "<string>",
  "streams": ["video", "audio", "telemetry"]
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"handshake"` |
| `protocol_version` | yes | Integer wire protocol version. Currently `1`. The server MUST close the connection if it does not support the declared version. |
| `fw_version` | yes | Camera firmware version string |
| `streams` | yes | Array of currently active stream names. Valid values: `"video"`, `"audio"`, `"telemetry"`. |

The camera does not declare its identity in the handshake — the server has already derived it from the device certificate public key fingerprint at the TLS layer before any application data is exchanged.

### 6.2 `capability_update`

Sent whenever the camera starts or stops video or audio capture mid-session. The `capability_update` MUST be sent and flushed on the Alerts stream before the camera begins writing to the corresponding video or audio stream.

```json
{
  "type": "capability_update",
  "streams": ["telemetry"]
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"capability_update"` |
| `streams` | yes | Full current active stream set (not a diff). |

### 6.3 `recording_segment`

Sent on connect (for any segments not yet persisted by the server) and after each newly finalised segment. Used by the server to maintain a Redis index of available footage for playback queries. Acknowledgement is implicit — the camera re-sends all unacknowledged segments on every reconnect; the server performs idempotent upserts to Redis. See `playback.md` for how this metadata is used.

```json
{
  "type": "recording_segment",
  "device_id": "<string>",
  "segment_id": "<device_id>:<start_ts>",
  "start_ts": 1700000000000,
  "end_ts": 1700000010000,
  "size_bytes": 1048576
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"recording_segment"` |
| `device_id` | yes | Device identifier |
| `segment_id` | yes | Globally unique segment key: `{device_id}:{start_ts}`. Used as the Redis key for idempotent upserts. |
| `start_ts` | yes | Segment start, Unix ms |
| `end_ts` | yes | Segment end, Unix ms |
| `size_bytes` | yes | Segment file size in bytes |

### 6.4 `segment_evicted`

Sent when the ring buffer evicts a segment to reclaim space. The server tombstones the corresponding Redis entry.

```json
{
  "type": "segment_evicted",
  "segment_id": "<device_id>:<start_ts>"
}
```

### 6.5 `segment_uploaded`

Sent after the camera successfully completes a segment upload stream. The `seq` correlates to the `upload_segment` command that triggered the upload.

```json
{
  "type": "segment_uploaded",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"segment_uploaded"` |
| `seq` | yes | The `seq` value from the `upload_segment` command |
| `segment_id` | yes | The segment that was uploaded |

### 6.6 `segment_upload_failed`

Sent when the camera cannot complete a segment upload. The `seq` correlates to the `upload_segment` command.

```json
{
  "type": "segment_upload_failed",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000",
  "reason": "evicted"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"segment_upload_failed"` |
| `seq` | yes | The `seq` value from the `upload_segment` command |
| `segment_id` | yes | The segment that failed to upload |
| `reason` | yes | `"evicted"` — segment was evicted before upload completed; `"not_found"` — segment ID not recognised; `"io_error"` — local filesystem read error |

### 6.7 `ack`

Sent in response to commands that require confirmation. See `auth.md` for certificate lifecycle commands.

```json
{
  "type": "ack",
  "cmd": "cert_refresh",
  "seq": 42
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"ack"` |
| `cmd` | yes | The `type` field of the command being acknowledged |
| `seq` | yes | The `seq` value from the command being acknowledged |

### 6.8 `csr`

Sent by the camera during enrollment as the first message on the Alerts stream. Carries a PEM-encoded Certificate Signing Request generated from a locally generated key pair. The server signs the CSR and returns the issued certificate via `cert_refresh`. The private key never leaves the camera.

```json
{
  "type": "csr",
  "csr_pem": "<PEM-encoded CSR>"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"csr"` |
| `csr_pem` | yes | PEM-encoded Certificate Signing Request. The private key never leaves the camera. |

### 6.9 `storage_full`

Sent when the camera's data partition is full and recording has paused. Emergency eviction of oldest segments was attempted but insufficient space was recovered. Live streaming is unaffected. Recording resumes automatically if free space becomes available.

```json
{
  "type": "storage_full",
  "free_bytes": 1048576
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"storage_full"` |
| `free_bytes` | yes | Bytes available on the data partition at time of failure |

### 6.10 `storage_resumed`

Sent when the camera resumes recording after a `storage_full` condition — free space has been recovered by operator intervention.

```json
{
  "type": "storage_resumed",
  "free_bytes": 5368709120
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"storage_resumed"` |
| `free_bytes` | yes | Bytes available on the data partition at time of resumption |

### 6.11 `update_applying`

Sent immediately before the camera reboots to apply a firmware update. After this alert the QUIC connection will close. The server should not treat the subsequent disconnect as an error.

```json
{
  "type": "update_applying",
  "version": "1.2.3"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"update_applying"` |
| `version` | yes | The firmware version being applied |

### 6.12 `update_succeeded`

Sent on the first successful QUIC connection after a firmware update. Signals that the new firmware is healthy and the watchdog timer has been cleared.

```json
{
  "type": "update_succeeded",
  "version": "1.2.3"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"update_succeeded"` |
| `version` | yes | The firmware version now running |

### 6.13 `update_failed`

Sent when a firmware update fails — either before applying (hash mismatch, download failure) or after applying (watchdog rollback). The camera is running the previous firmware version.

```json
{
  "type": "update_failed",
  "version_attempted": "1.2.3",
  "version_current": "1.2.2",
  "reason": "watchdog"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"update_failed"` |
| `version_attempted` | yes | The firmware version that failed to apply |
| `version_current` | yes | The firmware version currently running |
| `reason` | yes | `"watchdog"` — new firmware failed to connect within 5 minutes and was rolled back; `"hash_mismatch"` — downloaded binary SHA-256 did not match expected value; `"download_failed"` — binary could not be downloaded |

### 6.14 `networks`

Sent on connect and in response to a `list_networks` command. Reports the full list of WiFi networks currently stored in `/var/ghostcam/networks.conf`. Used by the server to serve the network management API.

```json
{
  "type": "networks",
  "networks": [
    { "ssid": "HomeNetwork", "signal_dbm": -62 },
    { "ssid": "OfficeNetwork", "signal_dbm": null }
  ]
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"networks"` |
| `networks` | yes | Array of known network entries |
| `networks[].ssid` | yes | Network SSID |
| `networks[].signal_dbm` | no | Current signal strength if the camera is connected to this network; `null` otherwise |

---

## 7. Commands Channel — Server → Camera (JSON)

All command messages include a `type` discriminant and a monotonically increasing `seq` number. The `seq` field is used to correlate ACKs for commands that require confirmation, and to correlate upload streams to their triggering commands.

| Command | ACK required | Description |
|---------|-------------|-------------|
| `start_video` | No | Begin live video capture and streaming |
| `stop_video` | No | Stop live video capture and streaming |
| `start_audio` | No | Begin live audio capture and streaming |
| `stop_audio` | No | Stop live audio capture and streaming |
| `upload_segment` | No (alert-based) | Upload a specific fMP4 segment (see `playback.md`) |
| `upload_init` | No | Upload the current fMP4 init segment (see `playback.md`) |
| `reboot` | Yes | Gracefully reboot the device |
| `network_config` | No | Add a WiFi network to the camera's known networks |
| `remove_network` | No | Remove a WiFi network from the camera's known networks |
| `list_networks` | No (alert-based) | Request current network list — camera responds with `networks` alert |
| `update_available` | No (alert-based) | Notify camera of available firmware update |
| `cert_refresh` | Yes | Deliver signed user association certificate during enrollment (see `auth.md`) |
| `unregister` | Yes | Clear user association cert and disconnect (see `auth.md`) |

The server sends `start_video`, `stop_video`, `start_audio`, and `stop_audio` implicitly based on live observer demand — these are not client-initiated commands. See `ingest.md` for subscriber demand tracking logic.

`upload_segment` completion is signalled via `segment_uploaded` or `segment_upload_failed` alerts rather than a direct ACK — see §6.5 and §6.6.

`list_networks` completion is signalled via a `networks` alert — see §6.14.

`update_available` completion is signalled via `update_applying`, `update_succeeded`, or `update_failed` alerts — see §6.11, §6.12, §6.13.

### 7.1 `start_video`

```json
{ "type": "start_video", "seq": 1 }
```

### 7.2 `stop_video`

```json
{ "type": "stop_video", "seq": 2 }
```

### 7.3 `start_audio`

```json
{ "type": "start_audio", "seq": 3 }
```

### 7.4 `stop_audio`

```json
{ "type": "stop_audio", "seq": 4 }
```

### 7.5 `upload_segment`

See `playback.md` for full upload flow context.

```json
{
  "type": "upload_segment",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000"
}
```

The camera opens a new outbound QUIC unidirectional stream, writes the raw `.m4s` file bytes, and closes the stream. On completion the camera sends `segment_uploaded`; on failure it sends `segment_upload_failed`.

### 7.6 `upload_init`

See `playback.md` for full upload flow context.

```json
{ "type": "upload_init", "seq": 11 }
```

The camera opens a new outbound QUIC unidirectional stream, writes the raw `init.mp4` bytes, and closes the stream. No alert is sent on completion — the server infers completion from stream close.

### 7.7 `reboot`

```json
{ "type": "reboot", "seq": 5 }
```

On receipt the camera sends `ack`, flushes any pending stream writes, then calls `systemctl reboot`.

### 7.9 `cert_refresh`

See `auth.md` for full lifecycle context. Carries the PEM-encoded user association certificate issued from the camera's CSR during enrollment. Also carries the server CA certificate on the initial enrollment — the camera pins this to verify future connections. The private key was generated locally by the camera and never transmitted.

```json
{
  "type": "cert_refresh",
  "seq": 7,
  "cert_pem": "<PEM-encoded user association certificate>",
  "ca_pem": "<PEM-encoded server CA certificate>"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"cert_refresh"` |
| `seq` | yes | |
| `cert_pem` | yes | PEM-encoded user association certificate |
| `ca_pem` | yes (enrollment only) | PEM-encoded server CA certificate. Present only during the initial enrollment. The camera stores this to verify future connections. Absent on any future certificate rotation. |

The camera MUST store the certificate and CA cert durably and send an `ack` before the server marks the device as enrolled.

### 7.10 `unregister`

See `auth.md` for full lifecycle context.

```json
{ "type": "unregister", "seq": 8 }
```

On receipt the camera sends `ack`, clears the user association certificate and key, clears the CA cert, server address, and server TLS pin, closes the QUIC connection, and enters registration mode.

### 7.11 `network_config`

Adds a WiFi network to the camera's known networks. The camera appends the new network to `/var/ghostcam/networks.conf` and updates NetworkManager. Existing networks are unaffected.

```json
{
  "type": "network_config",
  "seq": 9,
  "ssid": "OfficeNetwork",
  "psk": "password123"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"network_config"` |
| `seq` | yes | |
| `ssid` | yes | Network SSID to add |
| `psk` | yes | WPA2 pre-shared key |

### 7.12 `remove_network`

Removes a WiFi network from the camera's known networks. The camera removes the entry from `/var/ghostcam/networks.conf` and updates NetworkManager.

```json
{
  "type": "remove_network",
  "seq": 10,
  "ssid": "OldNetwork"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"remove_network"` |
| `seq` | yes | |
| `ssid` | yes | Network SSID to remove |

### 7.13 `list_networks`

Requests the camera's current known network list. The camera responds with a `networks` alert (see §6.14).

```json
{ "type": "list_networks", "seq": 11 }
```

### 7.14 `update_available`

Notifies the camera of an available firmware update. The camera downloads the binary, verifies the SHA-256 hash, and applies the update via the watchdog-supervised update flow (see `camera-firmware.md` §11).

```json
{
  "type": "update_available",
  "seq": 12,
  "version": "1.2.3",
  "url": "https://releases.ghostcam.io/firmware/1.2.3/ghostcam-camera-aarch64.bin",
  "sha256": "<64-character hex string>",
  "force": false
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `type` | yes | `"update_available"` |
| `seq` | yes | |
| `version` | yes | Firmware version string |
| `url` | yes | HTTPS URL to download the firmware binary |
| `sha256` | yes | Expected SHA-256 hash of the binary, hex-encoded |
| `force` | no | If `true`, the camera applies the update immediately regardless of recording state. Default `false` — camera waits for the next segment boundary before proceeding. |

The camera sends `ack` immediately on receipt (acknowledging the command, not the outcome), then proceeds with the download. Outcome is reported via `update_applying`, `update_succeeded`, or `update_failed` alerts.

---

## 8. Connection Lifecycle

### 8.1 Camera startup sequence

1. Establish QUIC/mTLS connection to server, presenting device identity cert and user association cert.
2. Open three persistent outbound unidirectional streams: **Alerts**, **Video**, **Audio**.
3. Wait for server to open inbound **Commands** stream.
4. Send `handshake` on Alerts stream, declaring `protocol_version`, identity, and current capabilities.
5. Open a manifest push stream and write the current `playlist.m3u8` (see `playback.md` §4.3).
6. If the telemetry buffer contains undelivered entries, open a telemetry buffer upload stream and write all buffered entries (see §5.2).
7. Begin telemetry datagram loop.
8. Begin stream write loops for any active media (gated on `start_video` / `start_audio` commands).
9. Send `recording_segment` alerts for any segments not yet persisted by the server.

### 8.2 Reconnect behaviour

On connection loss the camera reconnects and repeats the startup sequence from step 1. The server reconstructs all slot state from the `handshake` message — no state is assumed to persist across connections. The manifest, any buffered telemetry, and any unacknowledged segment metadata are all re-pushed on every reconnect.

### 8.3 Registration mode

If the camera has no valid user association certificate on startup, it enters registration mode rather than the normal startup sequence. In registration mode the camera waits to receive an enrollment token delivered via QR code scanned by the user. It then presents the token alongside its device identity certificate in the QUIC connection to negotiate a user association certificate. Once the certificate is issued and stored, the camera exits registration mode and proceeds with the normal startup sequence. See `auth.md` for the full enrollment flow.

### 8.4 Keepalives

The camera sends QUIC keepalives at a 15-second interval. The server considers a camera disconnected if no keepalive or data is received within 30 seconds.

---

## 9. Open Questions

| Question | Notes |
|----------|-------|
| QUIC application error code for unenrolled device | After mTLS succeeds, if the device cert fingerprint has no record in the application database, the server must reject the connection with a QUIC application-layer error. A specific error code value needs to be assigned for this condition and any other application-layer rejection reasons. |
| `network_config` ACK | The `network_config` command carries WiFi credentials and is not acknowledged. If the camera applies a bad config it may lose connectivity with no recovery path short of physical access. Consider requiring an ACK and/or a rollback mechanism (e.g. revert to previous config if no reconnect within N seconds). |
