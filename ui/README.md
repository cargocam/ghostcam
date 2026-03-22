# Ghostcam UI
Svelte 5 single-page viewer for live and playback camera monitoring.

![Ghostcam viewer with 4 simulated cameras](assets/screenshot.png)

## Setup
```bash
bun install
bun run dev      # http://localhost:5173
bun run build
bun run check
```

Equivalent `npm run ...` commands also work after `npm install`.

## Dev-server integration
`vite.config.ts` proxies these paths to backend port `3000`:
- `/api`
- `/events`
- `/hls`

This matches the current `server-core` HTTP surface.

## Stack
- Svelte 5 (runes-based state model)
- Vite 6
- Tailwind CSS 4
- bits-ui + lucide-svelte
- Leaflet (map view)
- hls.js (playback path)

## High-level architecture
The UI uses a **per-camera connection model**:
- one `RTCPeerConnection` per online camera,
- one `EventSource` for global online/offline events,
- one central store (`transportStore`) coordinating auth/session lifecycle.

### Startup flow
`App.svelte` calls `transportStore.initialize()`:
1. check auth session by probing `/api/v1/cameras`,
2. fetch initial camera list (`GET /api/v1/cameras`),
3. start SSE (`GET /events`),
4. create `ConnectionManager`,
5. connect WebRTC for each currently-online camera.

### SSE flow
`sse.ts` listens for:
- `camera_online`
- `camera_offline`

`transportStore` applies these to `cameraStore`, opens/closes per-camera connections, and pushes alert entries.

### WebRTC flow per camera
`CameraConnection` (`webrtc.ts`) does:
1. create peer connection (ICE-lite assumptions),
2. add recv-only transceivers (video + audio),
3. create `telemetry` and `commands` data channels,
4. generate offer, strip `a=candidate` lines,
5. call `POST /api/v1/watch` with `device_id` + `sdp_offer`,
6. apply `sdp_answer`,
7. stream tracks + telemetry updates into stores.

On disconnect, `ConnectionManager` retries after `RECONNECT_DELAY_MS` while camera remains online.

## Client mode and server demand control
The `commands` data channel is used to send:
- `{"type":"client_mode","mode":"live|playback|map"}`

`App.svelte` wires scrubber mode changes to:
- broadcast mode updates to all cameras, or
- in focused `1+5` layout, set playback mode only for the focused camera.

This aligns server ingest demand (`start/stop video/audio`) with viewer intent.

## Playback model
Playback uses server HLS endpoints directly:
- manifest: `/hls/<device_id>/playlist.m3u8`
- init segment: `/hls/<device_id>/init.mp4`
- segment objects: `/hls/<device_id>/<segment_id>`

`HlsPlayer.svelte`:
- parses manifest window bounds from segment IDs,
- supports seek updates from scrubber time,
- attempts media-source recovery on certain append failures.

`scrubberStore` provides global live/playback timeline state and playback ticking.

## Telemetry model
- Live telemetry arrives via WebRTC `telemetry` data channel.
- Historical telemetry for dashboard/map playback modes comes from:
  - `GET /api/v1/telemetry/:device_id?from=&to=&limit=`
- `telemetry-history.ts` adds short-lived client caching + nearest-sample lookup helpers.

## State stores
- `transportStore`: auth/session bootstrap + SSE + connection manager lifecycle.
- `cameraStore`: camera list, stream refs, telemetry snapshots.
- `settingsStore`: theme/layout/view/mute preferences.
- `scrubberStore`: timeline mode + playhead.
- `videoStatsStore`: inbound RTP-derived resolution/codec/bitrate/drops.
- `alertsStore`: online/offline alert feed.
- `cameraConfigStore`: local display-name overrides.

## Known compatibility stubs in current UI code
- `signaling.ts::listGroups()` targets `/api/v1/groups`; server route is not currently present, and callers already treat it as optional.
- `playback.ts` exposes legacy `/api/v1/cameras/:id/playback` helpers that are not used by current views (playback uses `/hls/...` paths).
- `sendIceCandidate()` is defined for API completeness; current ICE-lite flow does not depend on active trickle exchange.
