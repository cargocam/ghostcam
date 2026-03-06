# Implementation Plan: Issue #17 (Audio Pipeline) & Issue #9 (Audit Logging)

## Current State Assessment

### Issue #17 — Audio Pipeline: What Already Works

The server-side audio pipeline is **fully implemented**. After thorough codebase examination:

1. **Camera agent**: Both test-source (Opus silence every 20ms) and real capture (cpal + Opus encoding) send `CaptureMessage::Audio` frames via `send_audio_frame()` on QUIC uni-streams.
2. **Wire protocol**: `StreamType::Audio = 1` fully supported in 13-byte frame header.
3. **Server QUIC handler** (`server/src/quic.rs`): Audio frames decoded and forwarded to `router.on_audio_frame()`.
4. **Router broadcast** (`ghostcam/src/router.rs`): `on_audio_frame()` broadcasts `CameraFrame` with `StreamType::Audio`.
5. **WebRTC session creation** (`server/src/webrtc.rs`): Audio `MediaKind::Audio` tracks already added via `sdp_api.add_media()` for each camera. `audio_track_map` populated. Track mappings with `kind: "audio"` sent to viewer.
6. **WebRTC audio forwarding** (`server/src/webrtc.rs`): The `StreamType::Audio` arm computes Opus RTP timestamp, looks up audio Mid, finds Opus payload type, calls `writer.write()` with `Frequency::FORTY_EIGHT_KHZ`.

**Conclusion**: The entire server-side audio pipeline works. The issue is **viewer-side only**.

### What's Missing in the Viewer

1. **`ontrack` handler filters out audio** (`ui/src/lib/webrtc.ts` ~line 41): `if (track.kind !== 'video') return;`
2. **No audio stream storage**: `CameraState` only has `stream?: MediaStream` (video).
3. **`<video>` always muted**: Hardcoded `muted` without reactivity.
4. **No per-camera mute state**: Mute toggle directly toggles `videoElement.muted`, no persistent state.
5. **Audio stats not collected**: `pollStats()` only processes `kind === 'video'` inbound-rtp.

### Issue #9 — Audit Logging: Current State

Server uses `tracing` with `tracing_subscriber::fmt()` and `EnvFilter`. Existing log events at info level for QUIC connects/disconnects, router registration, WebRTC sessions. **No structured audit events, no HMAC integrity, no `/metrics` endpoint.**

---

## Issue #17: Audio Pipeline Implementation

### Step 1: Handle audio tracks in `ui/src/lib/webrtc.ts`

- Add `OnAudioTrackCallback` type and `onAudioTrack` field.
- Change `trackDeviceMap` from `Map<string, string>` to `Map<string, {device_id: string, kind: string}>`.
- Remove `if (track.kind !== 'video') return;` guard in `ontrack` handler.
- Route video tracks to existing `onTrack` callback, audio tracks to new `onAudioTrack`.
- Buffer audio tracks in `pendingAudioStreams` if track_map not yet available.
- Update `handleTrackMap()` to flush pending audio streams.

### Step 2: Store audio streams in `ui/src/lib/stores/cameras.svelte.ts`

- Add `audioStream?: MediaStream` to `CameraState`.
- Add `setAudioStream(deviceId, stream)` method mirroring `setStream()`.
- Clear `audioStream` in `removeCamera()`.

### Step 3: Wire audio callback in `ui/src/lib/stores/transport.svelte.ts`

- In `connect()`, set `this.session.onAudioTrack = (deviceId, stream) => cameraStore.setAudioStream(deviceId, stream);`

### Step 4: Combine video+audio in `ui/src/lib/components/VideoPlayer.svelte`

- Derive `audioStream` from camera state.
- In the `$effect` that sets `srcObject`, combine video and audio tracks into a single `MediaStream`.
- Compare track IDs to avoid unnecessary re-assignment (prevents playback restart).
- Remove hardcoded `muted`, bind reactively to mute state.

### Step 5: Global and per-camera mute in `ui/src/lib/stores/settings.svelte.ts`

- Add `globalMuted = $state(true)` (default muted for browser autoplay policy).
- Add `unmutedCameraId = $state<string | null>(null)` — only one camera unmuted at a time (prevents audio cacophony, standard VMS pattern).
- Methods: `toggleGlobalMute()`, `toggleCameraMute(deviceId)`, `isCameraMuted(deviceId)`.
- Persist to `localStorage` key `ghostcam-muted`.

### Step 6: Update mute controls in CameraCard and CameraView

- **`CameraCard.svelte`**: `toggleMute()` calls `settingsStore.toggleCameraMute(deviceId)`, derive icon from `settingsStore.isCameraMuted(deviceId)`.
- **`CameraView.svelte`**: Same pattern.
- **`VideoPlayer.svelte`**: Accept `isMuted` prop, sync `videoElement.muted` via `$effect`.

### Step 7: Audio stats collection in `transport.svelte.ts`

- In `pollStats()`, handle `stat.kind === 'audio'` inbound-rtp reports.
- Extract audio bitrate, packets received, jitter.

---

## Issue #9: Audit Logging Implementation

### Step 1: Create `ghostcam/src/audit.rs`

```rust
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum AuditEvent {
    AuthSuccess { remote_addr: String, path: String },
    AuthFailure { remote_addr: String, path: String },
    CameraConnect { device_id: String, group_id: String, remote_addr: String },
    CameraDisconnect { device_id: String, reason: String },
    ViewerSessionCreate { session_id: String, group_id: String, camera_count: usize },
    ViewerSessionDelete { session_id: String },
    GroupChange { device_id: String, old_group: String, new_group: String },
}

pub struct AuditEntry {
    pub timestamp: u64,       // unix millis
    pub seq: u64,             // monotonic sequence number
    pub event: AuditEvent,
    pub hmac: String,         // hex-encoded HMAC-SHA256
}

pub struct AuditLogger {
    hmac_key: hmac::Key,      // ring::hmac
    seq: AtomicU64,
    tx: broadcast::Sender<AuditEntry>,
}
```

- HMAC computed as: `HMAC-SHA256(key, "{seq}:{timestamp}:{event_json}")`.
- `log(&self, event)` creates entry, computes HMAC, broadcasts, and emits structured `tracing` event at `info` level with `target = "audit"`.
- Register in `ghostcam/src/lib.rs` as `pub mod audit;`.

### Step 2: Add dependencies

- Use `ring::hmac` (already a transitive dep of rustls — no new crate needed).
- Add `prometheus-client = "0.23"` to workspace deps for metrics.

### Step 3: Create `server/src/metrics.rs`

```rust
pub struct Metrics {
    pub registry: Registry,
    // Gauges
    pub active_cameras: Gauge,
    pub active_sessions: Gauge,
    // Counters
    pub camera_connections_total: Counter,
    pub camera_disconnections_total: Counter,
    pub viewer_sessions_total: Counter,
    pub auth_successes_total: Counter,
    pub auth_failures_total: Counter,
    pub video_frames_total: Counter,
    pub audio_frames_total: Counter,
    pub video_bytes_total: Counter,
    pub audio_bytes_total: Counter,
}
```

All prefixed with `ghostcam_`. Uses `prometheus-client` atomic types (lock-free).

### Step 4: Integrate in `server/src/main.rs`

- Add `--hmac-key` CLI arg (or `GHOSTCAM_HMAC_KEY` env, default for dev).
- Add `audit: Arc<AuditLogger>` and `metrics: Arc<Metrics>` to `AppState`.

### Step 5: Emit audit events at key points

| Location | Event |
|----------|-------|
| `server/src/api.rs` auth middleware | `AuthSuccess` / `AuthFailure` |
| `server/src/quic.rs` after hello | `CameraConnect` |
| `server/src/quic.rs` on disconnect | `CameraDisconnect` |
| `server/src/webrtc.rs` session create | `ViewerSessionCreate` |
| `server/src/webrtc.rs` session delete | `ViewerSessionDelete` |

Auth middleware needs `AppState` access — use `axum::middleware::from_fn_with_state` (idiomatic for Axum 0.7).

### Step 6: Add `GET /metrics` endpoint

- Route alongside `/healthz` and `/readyz` (no auth required, following Prometheus convention).
- Returns `text/plain; version=0.0.4` Prometheus text format.

### Step 7: Instrument metrics at key points

- QUIC handler: inc `active_cameras` on connect, dec on disconnect, inc frame/byte counters.
- WebRTC: inc `active_sessions` on create, dec on delete.
- Auth middleware: inc success/failure counters.

---

## Implementation Order

### Phase 1: Audio Pipeline (#17)

| Step | Files |
|------|-------|
| 1.1 | `ui/src/lib/webrtc.ts` |
| 1.2 | `ui/src/lib/stores/cameras.svelte.ts` |
| 1.3 | `ui/src/lib/stores/transport.svelte.ts` |
| 1.4 | `ui/src/lib/components/VideoPlayer.svelte` |
| 1.5 | `ui/src/lib/stores/settings.svelte.ts` |
| 1.6 | `ui/src/lib/components/camera/CameraCard.svelte` |
| 1.7 | `ui/src/lib/views/CameraView.svelte` |
| 1.8 | `ui/src/lib/stores/transport.svelte.ts` (audio stats) |
| 1.9 | `bun run check && bun run build` |

### Phase 2: Audit Logging (#9)

| Step | Files |
|------|-------|
| 2.1 | `ghostcam/src/audit.rs` (new), `ghostcam/src/lib.rs` |
| 2.2 | `Cargo.toml` (workspace + ghostcam) |
| 2.3 | `server/src/metrics.rs` (new) |
| 2.4 | `Cargo.toml` (workspace + server) |
| 2.5 | `server/src/main.rs` |
| 2.6 | `server/src/quic.rs` |
| 2.7 | `server/src/webrtc.rs` |
| 2.8 | `server/src/api.rs` |
| 2.9 | `cargo test && cargo clippy` |

### Phase 3: Documentation

Update `CLAUDE.md` and `README.md` with audio forwarding, audit events, `/metrics` endpoint, new CLI flags, new dependencies.

---

## Key Design Decisions

- **One camera unmuted at a time**: Standard VMS pattern. `unmutedCameraId` in settings prevents audio cacophony.
- **MediaStream combination**: Combine video + audio tracks into one MediaStream on `<video>` element for synchronized playback without separate `<audio>` elements.
- **Per-entry HMAC** (not hash chain): Simpler, concurrent-safe. Monotonic sequence number detects deletions/reordering. Full hash chain can be added later.
- **`prometheus-client`**: Official Prometheus Rust client, simpler than `metrics` crate stack, no background threads.
- **`/metrics` unauthenticated**: Follows Prometheus convention. Network-level access control in production.
- **`ring::hmac`**: Already a transitive dep — no new crate needed.

## Potential Challenges

1. **Browser autoplay policy**: Must default to muted. When user unmutes, may need `videoElement.play()` call.
2. **MediaStream identity**: Must compare track IDs (not object identity) to avoid playback restart on re-render.
3. **Auth middleware state access**: Use `from_fn_with_state` for idiomatic Axum 0.7 audit event emission.
