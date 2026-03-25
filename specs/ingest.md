# Ghostcam — Server Ingest

**Status:** Draft

---

## 1. Overview

This document specifies the server-side ingest pipeline for Ghostcam cameras. It covers the QUIC connection accept loop, mTLS verification, `IngestSlot` structure and lifecycle, broadcast fan-out, routing registry, live subscriber demand tracking, and upload stream handling.

The wire protocol spoken over these connections is specified in `wire-protocol.md`. The playback upload mechanism is specified in `playback.md`. Certificate verification and enrollment are specified in `auth.md`. The egress side — how frames reach observers — is specified in `webrtc-client.md`.

---

## 2. QUIC Connection Accept Loop

The server listens for incoming QUIC connections using Quinn. All connections use mutual TLS — the server presents its server certificate and requires a client certificate from the camera.

On each new connection:

1. Verify the camera's device identity certificate against the Ghostcam CA
2. Check whether a user association certificate is present:
   - If absent — route to the enrollment handler (see `auth.md` §4); do not create an IngestSlot
   - If present — continue to step 3
3. Verify the user association certificate against the Ghostcam CA
4. Check the user association certificate serial number against the Redis revocation list (see `auth.md` §6); close the connection if revoked
5. Wait for the camera to open its outbound **Alerts**, **Video**, and **Audio** streams
6. Open the inbound **Commands** stream toward the camera
7. Wait for the `handshake` alert on the Alerts stream
8. Verify `protocol_version` in the handshake — close the connection if unsupported
9. Look up `device_id` from the handshake in the application database — close the connection with a QUIC application error if not enrolled (see `wire-protocol.md` §9)
10. Create an `IngestSlot` for this camera, keyed by `device_id`, and register it in the routing registry under the owning `user_id`

Steps 1–4 happen at the QUIC/TLS layer before any application data is exchanged. The database lookup at step 9 is the application-layer gate: a device with valid certificates but no enrollment record is rejected before a slot is created.

### 2.1 Enrollment handler

When a camera connects with only a device identity certificate (no user association certificate), the server routes the connection to the enrollment handler rather than the normal accept path. The enrollment handler:

1. Opens a Commands stream toward the camera
2. Waits for the camera to open an Alerts stream and send a `csr` alert
3. Signs the CSR and issues a `cert_refresh` command carrying the signed certificate
4. Waits for the camera's `ack`
5. Marks the device as enrolled in the application database (updates the `cameras` row)
6. Closes the connection

No `IngestSlot` is created during enrollment. See `auth.md` §4 for the full enrollment flow.

### 2.2 Commands stream error handling

If the Commands stream resets mid-session due to a write error, the server tears down the `IngestSlot` and closes the QUIC connection. The camera will reconnect and re-establish all state from the `handshake` message. This is consistent with the soft-state design — a slot without a functional Commands stream cannot issue `start_video`, `stop_video`, `upload_segment`, or certificate lifecycle commands and is effectively broken.

---

## 3. IngestSlot

Each connected camera is represented by an `IngestSlot`. The slot owns the camera's full ingest pipeline and runs independently of observer state.

```rust
struct IngestSlot {
    device_id: DeviceId,
    user_id: UserId,
    capabilities: Arc<RwLock<StreamCapabilities>>,
    video_tx: broadcast::Sender<VideoFrame>,
    audio_tx: broadcast::Sender<AudioFrame>,
    telemetry_tx: broadcast::Sender<TelemetryFrame>,
    manifest: Arc<RwLock<String>>,
    commands_tx: mpsc::Sender<Command>,
    video_subscribers: Arc<AtomicUsize>,
    audio_subscribers: Arc<AtomicUsize>,
}
```

All three broadcast channels are always present. If the camera is not currently sending video or audio, those channels have no traffic — broadcast sends are no-ops when there are no receivers. Broadcast channel capacity is fixed at 512 frames per channel. If a receiver falls behind by more than 512 frames, older frames are dropped for that receiver.

### 3.1 Lifecycle

- **Created** when the camera QUIC connection is established and the handshake is received
- **Runs continuously** regardless of observer count — the read loops keep the connection alive
- **Torn down** only when the camera disconnects or the QUIC connection resets

On teardown all subscribed `EgressHandle`s receive a closed-channel signal and close their `RTCPeerConnection`s. The routing registry entry is removed.

### 3.2 Read loops

The slot runs concurrent read loops, one per channel:

| Loop | Source | Action |
|------|--------|--------|
| Alerts | Alerts QUIC stream | Parse JSON, dispatch to alert handler |
| Video | Video QUIC stream | Read length-prefixed frames, broadcast to `video_tx` |
| Audio | Audio QUIC stream | Read length-prefixed frames, broadcast to `audio_tx` |
| Telemetry | QUIC datagrams | Decode MessagePack, concurrently broadcast to `telemetry_tx` and write to Redis |
| Upload streams | Inbound QUIC streams | Receive segment, init, manifest, and telemetry buffer uploads (see §6) |

All loops run concurrently. A fatal error on any loop (stream reset, connection close) tears down the slot.

---

## 4. Alert Handling

The slot dispatches on the `type` field of each alert message received on the Alerts stream.

| Alert type | Action |
|------------|--------|
| `handshake` | Already handled by the accept loop before slot creation — ignored if received again |
| `capability_update` | Update `capabilities`, notify all subscribed `EgressHandle`s via their capability channel |
| `recording_segment` | Upsert segment metadata to Redis keyed on `segment_id` |
| `segment_evicted` | Tombstone the Redis entry for the evicted `segment_id` |
| `segment_uploaded` | Update the in-flight segment map entry to `Buffered` state (see §6.1) |
| `segment_upload_failed` | Remove the in-flight segment map entry, return error to waiting clients |
| `ack` | Correlate by `seq`, resolve the pending command future |

---

## 5. Routing Registry

The server maintains two in-memory registries:

```rust
user_id → HashMap<DeviceId, Arc<IngestSlot>>   // cameras currently connected
user_id → Vec<EgressHandle>                     // observers currently watching
```

Both registries are protected by `RwLock`. Reads are cheap and frequent (every frame fan-out); writes occur only on connect and disconnect.

> **Single-instance constraint.** The routing registry is in-memory and process-local. It is not shared across server instances. Deploying multiple instances behind a load balancer will silently break camera-to-observer routing, SSE event delivery, and live subscriber demand tracking. Do not run more than one instance against the same database and Redis cluster until `multi-server.md` is implemented.

### 5.1 Slot registration

On slot creation the slot is inserted into `user_id → IngestSlot` under its `device_id`. If a slot already exists for that `device_id` (stale entry from a previous connection), it is torn down and replaced.

### 5.2 TTL and keepalives

Slots are kept alive as long as the QUIC connection is active. The camera sends QUIC keepalives every 15 seconds; Quinn detects connection loss within 30 seconds. There is no separate application-level TTL — slot lifetime is tied entirely to QUIC connection lifetime.

Observer entries in `user_id → EgressHandle` are kept alive as long as the WebRTC session is active. On session close the egress handle is removed and its broadcast receivers are dropped.

### 5.3 Server restart recovery

The registry is soft state. On server restart all cameras and observers reconnect and re-register. Durable state (telemetry history, segment metadata, revocation list) is recovered from Redis. The manifest is re-pushed by the camera as part of its startup sequence.

---

## 6. Upload Stream Handling

The server receives segment, init, manifest, and telemetry buffer upload streams as inbound QUIC streams opened by the camera. The server identifies the type of each inbound stream by context:

- **Segment upload** — follows an `upload_segment` command; at most one in flight per camera at any time
- **Init upload** — follows an `upload_init` command; at most one in flight per camera at any time
- **Manifest push** — camera-initiated, no preceding command; arrives after handshake and after each segment finalisation or eviction
- **Telemetry buffer upload** — camera-initiated, no preceding command; arrives after the manifest push on reconnect if the camera has buffered entries

The server correlates segment and init uploads to their triggering commands by arrival order — at most one of each is in flight simultaneously per camera, so no stream-level header is needed. Manifest and telemetry buffer uploads require no correlation.

### 6.1 In-flight segment map

The server maintains a per-camera in-memory map for segment upload coalescing:

```rust
segment_id → SegmentState

enum SegmentState {
    Uploading { waiters: Vec<oneshot::Sender<Bytes>> },
    Buffered  { data: Bytes, expires_at: Instant },
}
```

When a segment is requested:

1. Check the map for an existing entry
   - `Uploading` — add the requester to the waiters list; it will be notified when the upload completes
   - `Buffered` — serve immediately from `data`; reset `expires_at` to 60 seconds from now
   - Absent — issue `upload_segment` command to camera, insert `Uploading` entry, add requester to waiters
2. When the upload stream closes, move the entry to `Buffered`, notify all waiters
3. After 60 seconds of inactivity the entry is evicted from the map

### 6.2 Manifest push streams

Inbound manifest push streams are camera-initiated — no command precedes them. The server reads the full stream into memory and replaces the slot's in-memory manifest on stream close. No correlation or acknowledgement is needed.

### 6.3 Telemetry buffer upload streams

Inbound telemetry buffer upload streams are camera-initiated — no command precedes them. The server reads the full stream, decodes the MessagePack array, and persists each entry to Redis using the entry's `ts` field as the authoritative timestamp. Entries are written via `XADD` using `server_ts` as the stream ID for monotonicity. See `telemetry.md` for the full persistence model.

---

## 7. Broadcast Fan-out

The slot's three `broadcast::Sender`s distribute frames to all subscribed `EgressHandle`s.

```
IngestSlot
  video_tx --> EgressHandle A (video_rx)
           --> EgressHandle B (video_rx)
           --> EgressHandle C (video_rx)

  audio_tx --> EgressHandle A (audio_rx)
           --> EgressHandle B (audio_rx)
           --> EgressHandle C (audio_rx)

  telemetry_tx --> EgressHandle A (telemetry_rx)
               --> EgressHandle B (telemetry_rx)
               --> EgressHandle C (telemetry_rx)
```

**Fan-out cost:** The slot sends once per frame regardless of observer count. Frame data is cloned per receiver — this is unavoidable but bounded by the number of active egress handles. When no observers are subscribed, `broadcast::send` returns immediately with no data copied.

**Slow receiver policy:** Broadcast channel capacity is fixed at 512 frames per channel. If an `EgressHandle`'s receiver falls behind by more than 512 frames, older frames are dropped for that receiver only. Other receivers are unaffected.

---

## 8. Live Subscriber Demand Tracking

The server tracks live video and audio subscriber counts per camera using atomic counters on the `IngestSlot`. Counts are updated whenever an `EgressHandle` receives a `client_mode` message on its reliable commands data channel.

```
client_mode: "live"     -> increment video_subscribers, increment audio_subscribers
client_mode: "playback" -> decrement video_subscribers, decrement audio_subscribers
client_mode: "map"      -> decrement video_subscribers, decrement audio_subscribers
```

When `video_subscribers` transitions from 0 → 1, the server sends `start_video` to the camera via the Commands stream. When it transitions from 1 → 0, it sends `stop_video`. The same logic applies to `audio_subscribers` independently.

Commands are sent exactly once per transition — the server does not re-send `start_video` if it is already streaming. The atomic counter ensures correct behaviour under concurrent mode changes from multiple clients.

`client_mode` messages arrive on a reliable ordered data channel (see `webrtc-client.md` §4), ensuring subscriber counts remain accurate even under lossy network conditions.

---

## 9. Failure Modes

### 9.1 Database unavailable at startup

The server cannot verify camera enrollment records or authenticate API requests without the application database. This is not a recoverable degraded state — serving unauthenticated connections would be a security hole.

**Policy:** fail hard. The server logs the error clearly and exits with a non-zero code. The systemd service will restart after `RestartSec`. The operator should investigate database connectivity before the server will start successfully.

### 9.2 Database unavailable mid-run

IngestSlots and EgressHandles in flight do not touch the database — they operate entirely from in-memory state. The database is only consulted for new camera QUIC connections (enrollment lookup) and new HTTP API requests (authentication).

**Policy:** keep existing sessions alive; reject new connections gracefully.

- New camera QUIC connections are rejected after mTLS succeeds — the server cannot verify enrollment without the database. The QUIC connection is closed with the unenrolled-device application error code (see `wire-protocol.md` §9).
- New HTTP API requests return `503 Service Unavailable` with a `Retry-After` header.
- Existing IngestSlots and EgressHandles continue unaffected — live streaming, segment uploads, and telemetry fan-out all proceed normally.
- The server retries the database connection in the background with exponential backoff (1s initial, capped at 30s). On recovery, new connections are accepted immediately.
- A warning is logged on each failed retry attempt.

### 9.3 Redis unavailable at startup

Redis holds the certificate revocation list and is the write target for telemetry. Unlike the database, Redis unavailability at startup does not prevent the server from functioning securely for most operations.

**Policy:** fail open with warning. The server starts normally and logs a prominent warning. The in-memory revocation cache (see §9.4) starts empty. Telemetry writes are dropped until Redis recovers.

### 9.4 In-memory revocation cache

The server maintains an in-memory cache of revoked certificate serial numbers to avoid a Redis lookup on every incoming QUIC connection.

- Populated from Redis on startup (if Redis is available — starts empty if not)
- Refreshed every 60 seconds via `SMEMBERS revoked_certs`
- New camera connections check the cache, not Redis directly
- A serial number absent from the cache is treated as not revoked — consistent with the fail-open policy
- When Redis recovers, the next 60-second refresh cycle repopulates the cache

This means a revoked certificate can connect during a Redis outage window of up to 60 seconds plus the outage duration. This is an accepted tradeoff for availability — the revocation list is for cameras that have been explicitly unregistered by their owner, not for active attack mitigation.

### 9.5 Redis unavailable mid-run

**Telemetry writes:** dropped silently. An error counter is incremented and logged at warn level. The telemetry fan-out to WebRTC egress handles continues unaffected — only the Redis persistence path drops.

**Revocation checks:** served from the in-memory cache (see §9.4). Stale by up to 60 seconds plus outage duration.

**Recovery:** the background refresh task retries Redis with exponential backoff. On recovery, telemetry writes resume and the revocation cache is refreshed immediately.

---

## 10. Open Questions

| Question | Notes |
|----------|-------|
| Concurrent `upload_init` and `upload_segment` | The spec states at most one segment upload and at most one init upload are in flight per camera at any time, and correlates each to its triggering command by arrival order. It does not address what happens if an `upload_init` and an `upload_segment` stream arrive concurrently. A stream-level type header, or a prohibition on issuing both commands simultaneously, is needed to disambiguate. |
| Bandwidth contention between live streaming and segment upload | A segment upload over the QUIC connection competes for bandwidth with the live video and audio streams. Under constrained connections (e.g. cellular) this could degrade live stream quality. Whether the camera should throttle upload streams, and how, is unspecified. |
