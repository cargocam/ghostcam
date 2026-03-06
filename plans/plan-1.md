# Implementation Plan: Issue #1 — Dynamic Renegotiation

## Overview

When a camera joins or leaves a group while a viewer is watching that group, the bridge must dynamically add or remove WebRTC media tracks on all active sessions without tearing down the PeerConnection. This requires SDP renegotiation initiated by the server: bridge creates an offer, sends it to the viewer via data channel, viewer responds with an answer via data channel.

Currently, `POST /watch` creates a session with tracks for all cameras present at that moment. Camera join/leave events are defined in the protocol (`camera_join`, `camera_leave`, `renegotiate` data channel messages) but the server never sends them, and the viewer handles `camera_join` by doing a full reconnect.

## Current State Analysis

**Server (`server/src/webrtc.rs`)**:
- `create_session()` uses `rtc.sdp_api()` then `sdp_api.accept_offer(remote_offer)`.
- Tracks added via `sdp_api.add_media(kind, dir, stream_id, track_id)` per camera.
- `RtcSession` stores `video_track_map: HashMap<DeviceId, Mid>` and `audio_track_map`.
- The `run()` loop handles commands, UDP, camera frames, ticks. No event path for camera join/leave.

**Server (`server/src/quic.rs`)**:
- `handle_camera_connection()` calls `router.register_camera()` on connect and `router.unregister_camera()` on disconnect.
- These do NOT notify the WebRTC engine.

**Shared lib (`ghostcam/src/router.rs`)**:
- `GroupRouter` has `cameras`, `groups`, `viewers` maps plus `frame_tx` broadcast channel.
- No event channel for camera join/leave notifications.

**Shared lib (`ghostcam/src/data_channel.rs`)**:
- `Renegotiate { sdp_offer }` defined for server-to-viewer.
- `CameraJoin`, `CameraLeave` defined but never sent.
- No variant for viewer-to-server SDP answer.

**Viewer (`ui/src/lib/webrtc.ts`)**:
- No handler for incoming renegotiation offers.

**Viewer (`ui/src/lib/data-channel.ts`)**:
- `camera_join` and `renegotiate` both call `transportStore.reconnect()` (full teardown).

**str0m 0.6 renegotiation API**:
1. `sdp_api.add_media(kind, dir, stream_id, track_id)` → returns `Mid`
2. `sdp_api.set_direction(mid, Direction::Inactive)` → disable a track
3. `let (offer, pending) = sdp_api.apply().unwrap()` → creates `SdpOffer` + `SdpPendingOffer`
4. `offer.to_sdp_string()` → serialize
5. `SdpAnswer::from_sdp_string(&s)` → parse viewer's answer
6. `rtc.sdp_api().accept_answer(pending, answer)` → finalize

**Key constraint**: Only one `SdpPendingOffer` at a time per str0m. Must serialize renegotiations.

---

## Architecture: Event Flow

### Camera Join

```
Camera                     Server                           Browser
  │                          │                                 │
  │── QUIC connect ────────→ │                                 │
  │── DeviceHello ─────────→ │ router.register_camera()        │
  │                          │ event_tx.send(CameraJoined)     │
  │                          │                                 │
  │                          │ WebRtcEngine receives event     │
  │                          │ for each session in group:      │
  │                          │   sdp_api.add_media(video)      │
  │                          │   sdp_api.add_media(audio)      │
  │                          │   (offer, pending) = apply()    │
  │                          │   store pending in session      │
  │                          │   send camera_join on DC        │
  │                          │   send renegotiate on DC ─────→ │ setRemoteDescription(offer)
  │                          │                                 │ createAnswer()
  │                          │                                 │ send sdp_answer on DC
  │                          │ ←── DC: sdp_answer ────────── │
  │                          │ accept_answer(pending, answer)  │
  │                          │ update track maps               │
  │                          │ send track_map on DC ─────────→ │ handleTrackMap()
  │                          │                                 │ ontrack fires, maps to device
```

### Camera Leave

```
Camera                     Server                           Browser
  │                          │                                 │
  │── QUIC disconnect ─────→ │                                 │
  │                          │ router.unregister_camera()      │
  │                          │ event_tx.send(CameraLeft)       │
  │                          │                                 │
  │                          │ for each session in group:      │
  │                          │   set_direction(video_mid, Inactive)
  │                          │   set_direction(audio_mid, Inactive)
  │                          │   (offer, pending) = apply()    │
  │                          │   send camera_leave on DC       │
  │                          │   send renegotiate on DC ─────→ │ setRemoteDescription(offer)
  │                          │                                 │ createAnswer()
  │                          │ ←── DC: sdp_answer ────────── │
  │                          │ accept_answer(pending, answer)  │
  │                          │ remove from track maps          │
  │                          │ send track_map on DC ─────────→ │ track ended, remove stream
```

### Signaling Transport

SDP answers sent viewer→server via the existing WebRTC data channel (bidirectional). Avoids adding a new HTTP endpoint and keeps all session-scoped signaling on the same transport.

---

## Phase 1: Camera Event Notification Channel

### Step 1.1: Add `CameraEvent` enum and broadcast channel to `GroupRouter` (`ghostcam/src/router.rs`)

```rust
#[derive(Debug, Clone)]
pub enum CameraEvent {
    Joined {
        device_id: DeviceId,
        group_id: GroupId,
        capabilities: Vec<String>,
    },
    Left {
        device_id: DeviceId,
        group_id: GroupId,
    },
}
```

Add `pub event_tx: broadcast::Sender<CameraEvent>` to `GroupRouter`. Initialize with `broadcast::channel(256)`.

### Step 1.2: Emit events from `register_camera` and `unregister_camera`

- In `register_camera()`: `let _ = self.event_tx.send(CameraEvent::Joined { ... });`
- In `unregister_camera()`: Capture `group_id` before removal, then `let _ = self.event_tx.send(CameraEvent::Left { ... });`

### Step 1.3: Subscribe from WebRTC engine (`server/src/main.rs`)

```rust
let camera_event_rx = group_router.event_tx.subscribe();
```

Pass to `WebRtcEngine::new()`.

---

## Phase 2: Data Channel Message Types

### Step 2.1: Add `SdpAnswer` variant to `DataChannelMessage` (`ghostcam/src/data_channel.rs`)

```rust
SdpAnswer { sdp_answer: String },
```

Makes `DataChannelMessage` bidirectional:
- Server→viewer: `Cameras`, `CameraJoin`, `CameraLeave`, `Telemetry`, `Renegotiate`, `TrackMap`
- Viewer→server: `SdpAnswer`

### Step 2.2: Update TypeScript types (`ui/src/lib/types.ts`)

Add `| { type: 'sdp_answer'; sdp_answer: string }` to the union.

---

## Phase 3: Server-Side Renegotiation (largest phase)

### Step 3.1: Extend `RtcSession` (`server/src/webrtc.rs`)

```rust
struct RtcSession {
    // ... existing fields ...
    pending_offer: Option<SdpPendingOffer>,
    pending_offer_timestamp: Option<Instant>,
    pending_track_additions: Vec<(DeviceId, Mid, Mid)>,  // (device_id, video_mid, audio_mid)
    pending_camera_events: Vec<CameraEvent>,
}
```

### Step 3.2: Add `camera_event_rx` to `WebRtcEngine`

```rust
pub struct WebRtcEngine {
    // ... existing ...
    camera_event_rx: broadcast::Receiver<CameraEvent>,
}
```

In `run()`, add to `tokio::select!`:
```rust
Ok(event) = self.camera_event_rx.recv() => {
    self.handle_camera_event(event);
}
```

### Step 3.3: Implement `handle_camera_event`

Find all sessions watching the affected group (including `__all__` meta-group), dispatch to `add_camera_to_session` or `remove_camera_from_session`.

### Step 3.4: Implement `add_camera_to_session`

1. If `pending_offer.is_some()`, queue to `pending_camera_events` and return.
2. Skip if camera already has tracks.
3. Send `camera_join` on data channel.
4. Call `sdp_api.add_media()` for video and audio, **capture returned Mids**.
5. Call `sdp_api.apply()` → `(offer, pending)`.
6. Store `pending` in session, store Mids in `pending_track_additions`.
7. Send `Renegotiate { sdp_offer }` on data channel.

### Step 3.5: Implement `remove_camera_from_session`

1. If `pending_offer.is_some()`, queue and return.
2. Get video/audio Mids from track maps.
3. Send `camera_leave` on data channel.
4. Call `sdp_api.set_direction(mid, Direction::Inactive)` for both.
5. Remove from track maps immediately (stop frame routing).
6. Call `sdp_api.apply()` → store pending, send offer.

### Step 3.6: Handle incoming SDP answers in `poll_all_sessions`

In `Event::ChannelData` handling, parse `DataChannelMessage::SdpAnswer`:

```rust
DataChannelMessage::SdpAnswer { sdp_answer } => {
    handle_sdp_answer_inline(session, &sdp_answer, ...);
}
```

### Step 3.7: Implement `handle_sdp_answer` logic

1. Take `pending_offer` from session (error if None).
2. Parse `SdpAnswer::from_sdp_string()`.
3. Call `rtc.sdp_api().accept_answer(pending, answer)`.
4. Apply `pending_track_additions` to track maps.
5. Send updated `track_map` on data channel.
6. If `pending_camera_events` is non-empty, mark for post-loop processing.

### Step 3.8: Extract `send_track_map` helper

Reusable function that builds `TrackMap` from `video_track_map` + `audio_track_map` and sends on data channel. Used in both initial session creation and renegotiation completion.

### Step 3.9: Process queued events after renegotiation completes

After the main poll loop, for sessions with queued events: drain events, re-dispatch to `add_camera_to_session`/`remove_camera_from_session`. Only the first proceeds (sets new `pending_offer`), rest re-queue — correct serialization.

---

## Phase 4: Viewer-Side Renegotiation

### Step 4.1: Add `handleRenegotiate` to `WebRtcSession` (`ui/src/lib/webrtc.ts`)

```typescript
async handleRenegotiate(sdpOffer: string): Promise<void> {
    if (!this.pc || !this.dataChannel) return;
    await this.pc.setRemoteDescription({ type: 'offer', sdp: sdpOffer });
    const answer = await this.pc.createAnswer();
    await this.pc.setLocalDescription(answer);
    this.dataChannel.send(JSON.stringify({
        type: 'sdp_answer',
        sdp_answer: this.pc.localDescription!.sdp,
    }));
}
```

### Step 4.2: Intercept `renegotiate` in `setupDataChannel`

Handle `renegotiate` messages internally in `webrtc.ts` instead of propagating to `data-channel.ts`:

```typescript
if (msg.type === 'renegotiate') {
    this.handleRenegotiate(msg.sdp_offer);
    return; // Don't propagate
}
```

### Step 4.3: Handle track ended/muted events

Add handlers to `pc.ontrack`:
```typescript
track.onended = () => { if (deviceId && this.onTrackEnded) this.onTrackEnded(deviceId); };
track.onmute = () => { /* similar */ };
```

Add `onTrackEnded: ((deviceId: string) => void) | null = null;` callback.

### Step 4.4: Update `data-channel.ts`

Remove `transportStore.reconnect()` from `camera_join` case. The `renegotiate` case is now intercepted in `webrtc.ts`.

### Step 4.5: Wire `onTrackEnded` in `TransportStore` (`transport.svelte.ts`)

```typescript
this.session.onTrackEnded = (deviceId) => cameraStore.removeStream(deviceId);
```

Add `removeStream(deviceId)` to camera store — sets `stream = undefined` without removing the camera entry.

---

## Phase 5: Edge Cases and Robustness

### 5.1: In-flight renegotiation

Handled by queuing: if `pending_offer.is_some()`, camera events go to `pending_camera_events`. Processed serially after each renegotiation completes.

### 5.2: Multiple cameras join simultaneously

Serialized: first triggers renegotiation, rest queued. Each completes before the next starts.

**Future optimization**: Batch multiple additions into a single renegotiation by coalescing queued events.

### 5.3: Viewer disconnects during renegotiation

Safety timeout: if `pending_offer` is set for >10s without answer, drop it, clear pending additions, log warning, process queued events.

```rust
if let Some(ts) = session.pending_offer_timestamp {
    if now.duration_since(ts) > Duration::from_secs(10) {
        warn!("renegotiation timed out");
        session.pending_offer = None;
        session.pending_track_additions.clear();
        // process queued events
    }
}
```

### 5.4: Data channel not yet open

If `data_channel_id.is_none()`, queue events. Process when `Event::ChannelOpen` fires.

### 5.5: NAL accumulator cleanup

On camera leave, remove the `nal_accumulator` entry for that device.

### 5.6: `__all__` group handling

Sessions watching `__all__` receive renegotiation for ANY camera join/leave:
```rust
.filter(|(_, s)| s.group_id == *group_id || s.group_id.0 == "__all__")
```

---

## Phase 6: Documentation

- Update `CLAUDE.md`: `renegotiate` message → "server-initiated SDP offer" (not stub), add `sdp_answer` row to data channel table, update architecture diagram, remove renegotiation TODO.
- Update `README.md`: same changes.
- Update sub-READMEs for server and UI.

---

## Dependency Graph

```
Phase 1 (Event Channel)            ── no deps
  1.1: CameraEvent enum
  1.2: Emit from register/unregister
  1.3: Subscribe in main.rs

Phase 2 (Data Channel Types)       ── no deps
  2.1: SdpAnswer variant (Rust)
  2.2: SdpAnswer (TypeScript)

Phase 3 (Server Renegotiation)     ── depends on Phase 1, 2
  3.1: RtcSession pending fields
  3.2: camera_event_rx in engine   ── depends on 1.3
  3.3: handle_camera_event         ── depends on 3.2
  3.4: add_camera_to_session       ── depends on 3.1, 3.3
  3.5: remove_camera_from_session  ── depends on 3.1, 3.3
  3.6: Handle SDP answers          ── depends on 2.1
  3.7: handle_sdp_answer logic     ── depends on 3.6
  3.8: send_track_map helper       ── depends on 3.7
  3.9: Process queued events       ── depends on 3.7

Phase 4 (Viewer Renegotiation)     ── depends on Phase 2
  4.1: handleRenegotiate           ── depends on 2.2
  4.2: Intercept in setupDataChannel
  4.3: Track ended handling
  4.4: Update data-channel.ts
  4.5: Wire onTrackEnded

Phase 5 (Edge Cases)               ── depends on Phase 3, 4
Phase 6 (Documentation)            ── depends on all
```

Phases 1 and 2 can be done in parallel. Phases 3 and 4 can be largely developed in parallel (server vs viewer).

---

## Files Modified

| File | Changes |
|------|---------|
| `ghostcam/src/router.rs` | `CameraEvent` enum, `event_tx` broadcast, emit from register/unregister |
| `ghostcam/src/data_channel.rs` | Add `SdpAnswer` variant |
| `server/src/main.rs` | Subscribe to `event_tx`, pass to `WebRtcEngine` |
| `server/src/webrtc.rs` | **Major**: `camera_event_rx`, pending offer state, `add_camera_to_session`, `remove_camera_from_session`, SDP answer handling, `send_track_map`, queued event processing, timeout |
| `ui/src/lib/webrtc.ts` | `handleRenegotiate`, `onTrackEnded`, intercept renegotiate in data channel, track ended/muted handlers |
| `ui/src/lib/data-channel.ts` | Remove `reconnect()` from `camera_join`, update `renegotiate` case |
| `ui/src/lib/types.ts` | Add `sdp_answer` to union |
| `ui/src/lib/stores/transport.svelte.ts` | Wire `onTrackEnded` |
| `ui/src/lib/stores/cameras.svelte.ts` | Add `removeStream` method |
| `CLAUDE.md`, `README.md` | Update data channel table, architecture, remove stub note |

---

## Testing Strategy

### Unit Tests
- `router.rs`: `register_camera` emits `CameraEvent::Joined`, `unregister_camera` emits `CameraEvent::Left`.
- `data_channel.rs`: Roundtrip `SdpAnswer` serialization.

### Manual Integration Tests
1. Server + viewer + `cam-01`. Verify video.
2. Start `cam-02` in same group → alert, renegotiation, `cam-02` appears **without** page reload.
3. Kill `cam-02` → alert, stream removed, `cam-01` continues.
4. `launch-cameras.sh 3 default` → all 3 appear via sequential renegotiations.
5. Kill all cameras → graceful removal of all streams.
6. Test `__all__` group: cameras in different groups all appear.

### Stress Test
- Rapidly start/stop cameras (every 2s) while viewer watches.
- Verify no memory leaks, no orphaned pending offers, no stuck sessions.
- Verify queuing correctly serializes renegotiations.

---

## Potential Challenges

1. **str0m `SdpPendingOffer` lifetime**: Only one at a time. Serialization via queue handles this.
2. **Track Mid discovery**: Captured at `add_media()` call time, stored in `pending_track_additions`, applied after `accept_answer`. More reliable than SDP re-parsing.
3. **Borrow checker in `poll_all_sessions`**: Need to collect data channel events before processing, since `handle_sdp_answer` needs `&mut session`. Handle inline within the iteration loop.
4. **Browser `ontrack` timing**: May fire before updated `track_map` arrives. Already handled by existing `pendingStreams` buffer.
5. **`Direction::Inactive` vs track removal**: str0m doesn't support removing media lines (SDP spec doesn't either). Setting to `Inactive` disables without removing. Mids are recycled if the same camera reconnects later (or new media lines are added).
