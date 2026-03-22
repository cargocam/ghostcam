# Ghostcam — WebRTC Client

**Status:** Draft

---

## 1. Overview

This document specifies the client-side WebRTC session model, signaling flow, data channel protocol, and live and playback player architecture.

View modes (grid view, focus view), timeline scrubber behaviour, playback controls, and audio focus model are specified in `ui.md`. The server-side ingest pipeline is specified in `ingest.md`. The playback upload and HLS serving mechanism is specified in `playback.md`. Historic telemetry queries are specified in `telemetry.md`. The wire protocol between camera and server is specified in `wire-protocol.md`.

The client is a TypeScript Svelte SPA.

---

## 2. Session Model

Each camera the observer is watching is represented by one `RTCPeerConnection`. Sessions are not bundled — cameras have independent lifecycles and independent bandwidth estimation.

### 2.1 Track and channel layout

Every PeerConnection carries the following tracks and data channels regardless of the camera's current capability state:

| Channel | Type | Direction | Notes |
|---------|------|-----------|-------|
| Video | RTP video (H.264) | Server → Client | Silent until server sends frames |
| Audio | RTP audio (Opus) | Server → Client | Silent until server sends frames |
| Telemetry | SCTP data channel (unreliable, unordered) | Server → Client | High-frequency sensor readings |
| Commands | SCTP data channel (reliable, ordered) | Client → Server | Mode changes and client commands |

The telemetry and commands data channels are asymmetric by design. The telemetry channel carries high-frequency MessagePack-encoded sensor datagrams from the server where latest-value semantics apply and loss is acceptable. The commands channel carries infrequent but consequential JSON-encoded client messages where delivery must be guaranteed — a dropped `client_mode` message would leave the server's live subscriber count incorrect.

Video and audio tracks are negotiated at session start. If the camera is not currently streaming, the tracks carry no RTP packets — there is no bandwidth cost for silent tracks.

### 2.2 Renegotiation

Renegotiation never occurs. The session structure is fixed at connect time. Camera capability changes (video start/stop) are communicated via telemetry data channel messages, not via SDP renegotiation.

---

## 3. Signaling

WebRTC signaling uses a REST + SSE model. The client holds one persistent SSE connection for server-to-client push. All client-to-server signaling is REST.

```
GET    /events                    -> SSE stream (held open for lifetime of session)
POST   /api/v1/watch              -> body: SDP offer -> response: { session_id, sdp_answer }
DELETE /api/v1/session/{id}       -> teardown
POST   /api/v1/session/{id}/ice   -> trickle ICE candidate
```

### 3.1 Connection flow

1. Client creates `RTCPeerConnection` with video, audio, telemetry data channel, and commands data channel configured
2. Client waits for ICE gathering to complete — trickle ICE is disabled, the server has a fixed known candidate set
3. Client `POST /api/v1/watch` with the complete SDP offer and `device_id`
4. Server returns `{ session_id, sdp_answer }`; client calls `setRemoteDescription`
5. ICE connects, media flows

### 3.2 SSE stream

The SSE stream carries server-to-client push events scoped to the authenticated user. The client reconnects automatically on SSE disconnect.

| Event type | Payload | Description |
|------------|---------|-------------|
| `camera_online` | `{ device_id }` | Camera connected to server |
| `camera_offline` | `{ device_id }` | Camera disconnected from server |

On `camera_offline` the client closes the corresponding `RTCPeerConnection` and renders the tile in an offline state. On `camera_online` the client may re-establish a session if the tile is in view.

---

## 4. Data Channels

### 4.1 Telemetry channel — Server → Client (MessagePack, unreliable, unordered)

The camera transmits telemetry to the server as MessagePack-encoded datagrams over QUIC (see `wire-protocol.md` §5.1). The server decodes each datagram and re-encodes the merged telemetry state as a MessagePack map before forwarding it to subscribed egress handles over the WebRTC data channel. The client therefore receives MessagePack, not raw camera datagrams — the server may inject or omit fields relative to the original camera payload.

Datagrams are transmitted when any field crosses its threshold, or every 30 seconds as a full heartbeat — whichever comes first. All fields are optional — the client retains the last known value for any field absent from a datagram.

| Field | Description |
|-------|-------------|
| `ts` | Camera clock timestamp (Unix ms) |
| `sig` | WiFi signal strength (dBm) |
| `temp` | SoC temperature (°C) |
| `fps` | Current capture frame rate |
| `kbps` | Current video bitrate (kbps) |
| `lat`, `lon`, `alt` | GPS position. Absent if GPS hardware unavailable or no fix. |
| `gps_fix` | GPS fix quality (`0` = none, `1` = 2D, `2` = 3D). Absent if GPS hardware unavailable. |

See `wire-protocol.md` §5.1 for the full schema.

### 4.2 Commands channel — Client → Server (JSON, reliable, ordered)

The client sends JSON-encoded messages on the commands data channel. Reliable ordered delivery ensures the server's state remains consistent with the client's intent even under lossy network conditions.

#### `client_mode`

Sent on connect (declaring initial mode) and on every mode change.

```json
{ "type": "client_mode", "mode": "live" }
{ "type": "client_mode", "mode": "playback" }
{ "type": "client_mode", "mode": "map" }
```

| Mode | Description |
|------|-------------|
| `"live"` | Client is watching live video and audio. Counts toward the camera's live subscriber demand. |
| `"playback"` | Client is in HLS playback mode. Does not count toward live subscriber demand. |
| `"map"` | Client is viewing telemetry only. Does not count toward live subscriber demand. |

The server uses `client_mode` to manage implicit `start_video` / `stop_video` and `start_audio` / `stop_audio` commands to the camera. See `ingest.md` §8 for the demand tracking logic.

---

## 5. Player Architecture

The client maintains two players per camera behind a single unified viewport:

- **Live player** — `RTCPeerConnection` video and audio tracks rendered into a `<video>` element via `srcObject`
- **Playback player** — `<video>` element driven by hls.js or native HLS

Only one player is visible at a time. Both `<video>` elements occupy the same viewport position; visibility is toggled via CSS. No layout shift occurs on switch.

```
+-----------------------------------------+
|              Tile viewport              |
|  +-----------------------------------+  |
|  |   <video> live                    |  |  <- visible in live / map mode
|  |   srcObject=WebRTC                |  |
|  +-----------------------------------+  |
|  +-----------------------------------+  |
|  |   <video> playback                |  |  <- visible in playback mode
|  |   hls.js / native HLS             |  |
|  +-----------------------------------+  |
+-----------------------------------------+
```

### 5.1 HLS player detection

```typescript
function attachHls(video: HTMLVideoElement, deviceId: string) {
  const src = `/hls/${deviceId}/playlist.m3u8`;
  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = src;
  } else {
    const hls = new Hls();
    hls.loadSource(src);
    hls.attachMedia(video);
  }
}
```

### 5.2 Unavailability states

The tile renders one of the following states when video is unavailable:

| State | Condition |
|-------|-----------|
| Offline | SSE `camera_offline` event received |
| No signal | Camera online, `mode: "live"`, no video track activity |
| No footage | Playback requested, segment returned 404 and outside manifest window |
| Loading | Playback requested, segment upload in progress |

---

## 6. Mode Switching

### 6.1 Live → playback

1. Client sends `client_mode: "playback"` on the commands data channel
2. Client fetches `/hls/{device_id}/playlist.m3u8`
3. Client initialises hls.js with the manifest, seeking to the target timestamp
4. hls.js fetches init segment and first segment
5. Playback `<video>` becomes visible; live `<video>` is hidden
6. Live audio is muted (WebRTC audio track continues receiving but is not rendered)

### 6.2 Playback → live

1. Client sends `client_mode: "live"` on the commands data channel
2. Live `<video>` becomes visible; playback `<video>` is hidden
3. hls.js instance is destroyed
4. Live audio is unmuted

### 6.3 Scrub outside available window

If the client scrubs to a timestamp outside the camera's manifest window, the segment request returns 404. The client:

1. Re-fetches the manifest to get the current available window
2. Updates the tile to the no-footage unavailability state
3. Updates the timeline scrubber to reflect the actual available window for this camera

---

## 7. Timeline

The timeline scrubber displays the camera's available footage window and provides navigation between historic footage and live. See `ui.md` for full timeline behaviour across view modes (grid view, focus view).

### 7.1 Live mode

In live mode the scrubber shows the current time as a live indicator. There is no position dragging in live mode — the scrubber reflects live state, not a seekable position.

### 7.2 Entering playback

When the user clicks or drags to a position on the timeline, the player enters playback mode. A **Go Live** button appears at the trailing edge of the scrubber. Clicking Go Live sends `client_mode: "live"` on the commands data channel and returns the player to live mode.

### 7.3 Available window

The available footage window is derived from the camera's HLS manifest, fetched on session start and refreshed on each manifest push received from the server. The scrubber reflects this window — positions outside the available window are not scrubbable.

### 7.4 Clock skew

Cameras are independent devices. A global scrub to timestamp T seeks each camera to T on its own local clock. If cameras have clock drift relative to each other, footage from different tiles will not be perfectly frame-aligned at the same nominal timestamp. NTP or GPS-disciplined clocks on the cameras reduce but do not eliminate this. Clock skew is accepted as a known limitation.

---

## 8. Historic Telemetry

When the client enters playback mode it fetches historic telemetry for the visible timeline window via REST:

```
GET /telemetry/{device_id}?from={unix_ms}&to={unix_ms}
```

See `telemetry.md` for the full query API and response schema.

Live telemetry continues flowing over the telemetry data channel during playback and is displayed as current device state independently of the playback position. The client does not attempt to correlate live telemetry readings with the historic playback position — they are presented as separate data streams.

---

## 9. Multi-Camera Session Management

The client maintains a map of active `RTCPeerConnection`s keyed on `device_id`. Sessions are created lazily — a PeerConnection is established only when the user requests a feed for that camera.

On camera offline (SSE event):
- The corresponding PeerConnection is closed
- The tile renders the offline state
- Other cameras' PeerConnections are unaffected

On camera online (SSE event):
- If the tile is in view, the client re-establishes a PeerConnection
- The tile resumes live mode

Session teardown on observer navigate-away:
- All active PeerConnections are closed via `DELETE /api/v1/session/{id}`
- The SSE connection is closed
