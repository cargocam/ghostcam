# ingest

QUIC camera ingest pipeline. Accepts incoming camera connections, performs the mTLS handshake, reads the alerts stream, dispatches video/audio frames to broadcast channels, and manages per-camera state.

## Flow

```
camera connects (QUIC + mTLS)
    │
    ▼
accept::run_accept_loop()
    │
    ├─► verify client cert fingerprint against DB (enrolled + not revoked)
    │
    ├─► read Alert::Handshake from alerts stream
    │
    ├─► create IngestSlot (broadcast channels, demand watch, telemetry state)
    │
    ├─► register slot in RoutingRegistry
    │
    ├─► emit SseEvent::CameraOnline
    │
    └─► spawn per-stream readers
            ├── alerts::run_alerts_loop()   — reads Alert messages, routes to handlers
            ├── Video stream reader         — reads length-prefixed H.264 NALs → VideoFrame broadcast
            ├── Audio stream reader         — reads length-prefixed Opus frames → AudioFrame broadcast
            └── Upload stream acceptor      — accepts one-shot upload streams (segments, manifests, telemetry buffers)
```

On disconnect: slot is removed from registry, `SseEvent::CameraOffline` emitted.

## Files

| File | Purpose |
|------|---------|
| `accept.rs` | `run_accept_loop` — QUIC endpoint accept loop, connection auth, slot creation, application-level connection limit (QUIC_MAX_CONNECTIONS) |
| `slot.rs` | `IngestSlot` — per-camera state: broadcast channels (video/audio/telemetry), demand watch, segment ring buffer, HLS manifest |
| `registry.rs` | `RoutingRegistry` — concurrent map of `DeviceId → IngestSlot`, used by egress and API handlers |
| `alerts.rs` | Reads `Alert` messages from the persistent alerts QUIC stream; dispatches `RecordingSegment`, `Ack`, `CapabilityUpdate`, etc. |
| `demand.rs` | `ClientMode` enum (Live/Playback) and subscriber demand tracking — cameras pause streaming when no viewers are watching |
| `enrollment.rs` | Handles `Alert::Enrollment` — verifies JWT, issues signed device cert, writes camera record to DB |
| `uploads.rs` | Accepts one-shot QUIC upload streams (fMP4 segments, init segments, manifests, telemetry buffers); buffers in slot |
| `quic_config.rs` | `build_server_endpoint` — configures Quinn QUIC endpoint with mTLS using the server's CA. Limits concurrent bidi/uni streams per connection (QUIC_MAX_BIDI_STREAMS, QUIC_MAX_UNI_STREAMS) |
