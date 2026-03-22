# Yurei v2 Bridge Architecture

## Overview

The bridge is a protocol translator sitting between camera devices and browser viewers. Cameras connect over QUIC (Quinn); viewers connect over WebRTC (str0m). The bridge is not an SFU — it does not transcode, mix, or route media between viewers. It performs one job: forward encoded frames from a camera's ingest slot to any number of subscribed viewer egress handles.

The fundamental routing primitive is the **group ID** — a stable identifier tied to a user account. All cameras belonging to a user register under the same group ID. All viewers authenticated to that account attach to the same group ID. The bridge resolves `group_id + camera_id → ingest slot` and `group_id → [egress handles]`.

---

## Topology

```
Camera (Pi Zero 2W)
  │
  │  QUIC (Quinn)
  │  streams: video, audio, telemetry
  ▼
┌─────────────────────────────────────────┐
│               Bridge                    │
│                                         │
│  IngestSlot per camera                  │
│  ┌─────────────────────────────────┐    │
│  │ QUIC read loop                  │    │
│  │ demux: video / audio / telemetry│    │
│  │ ring buffer (1-2s)              │    │
│  │ keyframe cache                  │    │
│  │ broadcast::Sender<Frame>        │    │
│  └────────────┬────────────────────┘    │
│               │ fan-out                 │
│       ┌───────┼───────┐                 │
│       ▼       ▼       ▼                 │
│     Egress  Egress  Egress              │
│     Handle  Handle  Handle              │
│       │       │       │                 │
└───────┼───────┼───────┼─────────────────┘
        │       │       │
        ▼       ▼       ▼
    Viewer A  Viewer B  Viewer C
    WebRTC    WebRTC    WebRTC
```

---

## Ingest: Camera → Bridge

### Connection

Cameras connect to the bridge over QUIC. Each camera opens a single QUIC connection and multiplexes three unidirectional streams:

| Stream | Content | Reliability |
|--------|---------|-------------|
| Video  | H.264/H.265 NAL units | Loss-tolerant; bridge drops incomplete frames |
| Audio  | Opus frames | Loss-tolerant |
| Telemetry | MessagePack-encoded sparse updates | Unreliable, unordered |

On connect, the camera sends a handshake identifying its `group_id` and `camera_id`. The bridge creates an `IngestSlot` and begins reading all three streams in a dedicated async task.

### IngestSlot

The slot owns the camera's full ingest pipeline and runs independently of viewer state.

```rust
struct IngestSlot {
    group_id: GroupId,
    camera_id: CameraId,
    video_tx: broadcast::Sender<VideoFrame>,
    audio_tx: broadcast::Sender<AudioFrame>,
    telemetry_tx: broadcast::Sender<TelemetryFrame>,
    keyframe_cache: Arc<RwLock<Option<VideoFrame>>>,
    ring_buffer: RingBuffer<Frame>,
}
```

**Lifecycle:**
- Created when camera QUIC connection is established
- Runs its read loop continuously, regardless of viewer count
- When no viewers are subscribed, broadcast sends are no-ops (frames dropped cheaply)
- Torn down only when the camera disconnects or the QUIC connection resets

**Keyframe cache:** The slot retains the last complete I-frame. When a viewer subscribes mid-stream, the bridge immediately sends the cached keyframe before forwarding live frames. This eliminates the black-screen wait for the next GOP boundary, which at typical camera keyframe intervals (2-4s) is a meaningful UX improvement.

**Ring buffer:** A short rolling buffer (1-2 seconds) allows a briefly-lagging viewer egress handle to catch up without the slot needing to know about it.

---

## Egress: Bridge → Viewer

### WebRTC Session Structure

Each viewer establishes WebRTC connections directly to the bridge (no TURN relay — the bridge is a known server with a stable address). The session is structured as **one PeerConnection per camera**, not one PeerConnection with all tracks bundled.

**Rationale:**
- Camera lifecycle is independent — a camera going offline closes its PC without touching other feeds
- Bandwidth estimation, packet loss, and RTCP feedback are per-camera, preventing one bad stream from degrading others
- Viewers connect lazily — PCs are only established for cameras the viewer actually requests
- Renegotiation scope is minimal; adding or removing a camera doesn't touch existing sessions

**Track layout per PeerConnection:**

| Track | Type |
|-------|------|
| Video | RTP video track (H.264) |
| Audio | RTP audio track (Opus) |
| Telemetry | SCTP data channel (unreliable, unordered) |

Telemetry uses a WebRTC data channel rather than a media track. This provides clean framing, configurable reliability, and avoids any encoding overhead.

### EgressHandle

Each active viewer×camera pair is represented by an `EgressHandle`. It holds broadcast receivers for all three channels from the corresponding `IngestSlot` and drives the WebRTC send loop.

```rust
struct EgressHandle {
    peer_connection: RTCPeerConnection,
    video_rx: broadcast::Receiver<VideoFrame>,
    audio_rx: broadcast::Receiver<AudioFrame>,
    telemetry_rx: broadcast::Receiver<TelemetryFrame>,
}
```

**On creation:**
1. Bridge looks up `IngestSlot` by `group_id + camera_id`
2. Subscribes to slot's broadcast channels
3. Immediately sends cached keyframe to avoid stall
4. Begins forwarding live frames

**On viewer disconnect:**
- `EgressHandle` is dropped
- Broadcast receivers are dropped; slot is unaffected
- No renegotiation or cleanup required on the slot side

**On camera disconnect:**
- `IngestSlot` is torn down
- All `EgressHandle`s subscribed to that slot receive a channel closed signal
- Each handle closes its `RTCPeerConnection`
- Other cameras' PCs for the same viewer are unaffected

---

## Routing Registry

The bridge maintains two in-memory registries:

```
group_id → [IngestSlot]         // cameras currently connected
group_id → [EgressHandle]       // viewers currently watching
```

Both are soft state with TTL-based expiry. Devices send a QUIC keepalive; viewers send a heartbeat on a dedicated data channel. If the bridge restarts, neither registry is persisted — devices reconnect and re-register their slots; viewers re-request their feeds. The only meaningful state lost on restart is the keyframe cache, causing a brief stall on reconnect (acceptable).

---

## Fan-out Cost

M×N sends are unavoidable — every viewer watching every camera requires bytes delivered. The design minimizes overhead around that irreducible cost:

- **Ingest is O(M):** One QUIC read loop, one demux pipeline, one ring buffer per camera. The `broadcast::Sender` handles fan-out; the slot sends once per frame regardless of viewer count.
- **Egress is O(M×N) only at the send layer:** Each `EgressHandle` receives a clone of the frame and writes it to its WebRTC track. Frame data is cloned per viewer (unavoidable), but pipeline state is not.
- **Zero cost for unwatched cameras:** If no viewers are subscribed to a slot, broadcast sends return immediately. The slot keeps running to maintain connection state and refresh the keyframe cache, but no frame data is copied.

---

## Statefulness and Recovery

The bridge is designed to be reconstructible from device and viewer reconnections alone.

| State | Location | Recovery |
|-------|----------|---------|
| Camera registration | IngestSlot (memory) | Camera reconnects, slot recreated |
| Viewer session | EgressHandle (memory) | Viewer re-requests feed |
| Keyframe cache | IngestSlot (memory) | Lost on restart; viewer stalls until next I-frame |
| Ring buffer | IngestSlot (memory) | Lost on restart; acceptable |
| Auth tokens | Viewer holds | Re-presented on reconnect |

The bridge holds no durable state. Group IDs and camera IDs are validated against an external auth service on connect; the bridge does not need to store them persistently.

---

## Stack

| Layer | Crate |
|-------|-------|
| QUIC (camera ingest) | `quinn` |
| WebRTC (viewer egress) | `str0m` |
| HTTP / signaling | `axum` |
| TLS | `rustls` + `aws-lc-rs` (FIPS 140-3) |
| Async runtime | `tokio` |
| Frame serialization | `bytes` |
| Telemetry encoding | `rmp-serde` (MessagePack) |
| Frontend | TypeScript + React |
| Native viewer | Tauri (embeds web client) |

---

## Open Questions

- **Signaling transport:** WebRTC offer/answer exchange currently assumed over a dedicated `axum` WebSocket endpoint. Worth specifying the exact signaling message format early to avoid churn.
- **Keyframe cache durability:** For a future multi-bridge deployment, the keyframe cache would need to live in a sidecar (e.g., Redis) to survive bridge restarts without viewer stalls. Not needed at current scale.
- **Simulcast:** Deferred. If Pi Zero 2W bandwidth becomes a constraint, two spatial layers (preview + full) would allow the bridge to forward the appropriate layer per viewer without transcoding. Camera-side encoding complexity is the blocker.
- **CNSA 2.0 / post-quantum:** ML-KEM-1024 for key exchange, ML-DSA-87 for signatures, deferred to a later hardening phase. Current TLS stack (rustls + aws-lc-rs) is the foundation.