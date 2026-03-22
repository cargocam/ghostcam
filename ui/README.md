# Ghostcam Viewer

Svelte 5 SPA for the Ghostcam surveillance system. Provides live WebRTC video viewing, HLS playback of recorded footage with a timeline scrubber, a GPS map, telemetry dashboards, and camera management.

## Setup

```bash
bun install
bun run dev       # Dev server at http://localhost:5173
bun run build     # Production build → dist/
bun run check     # svelte-check type checking
```

The Vite dev server proxies `/api`, `/hls`, and `/events` to the server at `:3000`.

## Tech Stack

- **Svelte 5** — runes reactivity (`$state`, `$derived`, `$effect`)
- **Vite 6** — dev server and build
- **Tailwind CSS 4** — OKLCH color tokens defined in `app.css`
- **bits-ui 2** — headless component primitives (`components/ui/`)
- **lucide-svelte** — icons
- **Leaflet** — map integration
- **hls.js** — HLS playback for recorded footage

## Features

- Password-protected login (session cookie)
- Multi-camera grid — auto-fit and 1+5 featured layouts
- Live WebRTC video + audio with per-camera mute (one camera at a time)
- HLS playback mode — switches each camera card from WebRTC to HLS player
- Timeline scrubber — global playhead for navigating recorded footage and historical telemetry
- Telemetry history — fetch and display CPU, memory, temperature, GPS at any past point in time
- GPS map with camera markers and playback trail overlay
- Dashboard view with aggregate stats and sparkline charts
- Camera online/offline status, display name overrides (localStorage)
- Connection alerts (disconnect/reconnect notifications)
- Dark/light/system theme, mobile responsive (sidebar + drawer nav)

## Architecture

### Transport Layer

Camera events and state arrive via **Server-Sent Events** (`/events`), not WebRTC data channel. Each camera's live video and audio arrive via a separate **WebRTC** `RTCPeerConnection` (`/api/v1/watch`). Historical telemetry is fetched on demand via the **REST API** (`/api/v1/telemetry/:id`).

`connection-manager.ts` ties these together: on SSE `camera_online`, a `WebRtcSession` is created for that camera. On `camera_offline`, the session is torn down.

### Stores

| Store | File | Purpose |
|-------|------|---------|
| `transportStore` | `transport.svelte.ts` | SSE connection, WebRTC session map, authentication state |
| `cameraStore` | `cameras.svelte.ts` | Camera registry, live streams, telemetry, online status, selection |
| `scrubberStore` | `scrubber.svelte.ts` | Timeline mode (`live`/`playback`), playhead time, mode change callbacks |
| `groupStore` | `groups.svelte.ts` | Group list and active group |
| `settingsStore` | `settings.svelte.ts` | Theme, grid layout, view mode, mute state (localStorage) |
| `alertStore` | `alerts.svelte.ts` | Disconnect/reconnect event log |
| `cameraConfigStore` | `cameraConfig.svelte.ts` | Per-camera display name overrides (localStorage) |
| `videoStatsStore` | `videoStats.svelte.ts` | Per-track WebRTC inbound-rtp stats |
| `thumbnailStore` | `thumbnails.svelte.ts` | Canvas-captured frame thumbnails (data URLs) |

### Key Library Files

| File | Purpose |
|------|---------|
| `auth.ts` | Login, logout, session check |
| `sse.ts` | SSE client — parses events, drives cameraStore and transportStore |
| `signaling.ts` | `watchCamera` / `unwatchCamera` — WebRTC SDP exchange with server; `fetchTelemetryRangeCached` |
| `webrtc.ts` | `WebRtcSession` — `RTCPeerConnection` per camera, ICE candidate handling, `stripCandidates()` for Firefox mDNS compat |
| `connection-manager.ts` | Orchestrates SSE events → WebRTC session lifecycle |
| `playback.ts` | hls.js wrapper for HLS player |
| `telemetry-history.ts` | Fetch telemetry time ranges from API with in-memory cache; `nearestTelemetryEntryWithin` |

### Views

| View | Description |
|------|-------------|
| `LiveView` | Camera grid — WebRTC or HLS per card depending on scrubber mode. Online cameras sorted first. |
| `CameraView` | Single fullscreen camera — keyboard shortcuts F/M/S/P/Esc |
| `MapView` | Leaflet map with camera markers at live or historical GPS positions; playback trail overlay |
| `DashboardView` | Aggregate telemetry — stats panels and sparklines, historical data on scrub |

### Firefox WebRTC Note

Firefox obfuscates ICE candidates as mDNS hostnames (e.g. `a1b2c3.local`). `webrtc.ts` strips all `a=candidate` lines from the SDP offer before posting it to the server — safe because the server is ICE-lite and ignores browser candidates entirely. The server's `GHOSTCAM_PUBLIC_IP` must be a reachable LAN IP (not `127.0.0.1`) so Firefox can send STUN from its LAN-bound UDP socket.

## Conventions

- **Svelte 5 runes only** — no legacy `$:` reactivity
- **Tailwind CSS 4** — OKLCH tokens in `app.css`, `cn()` for class merging
- **localStorage** keys prefixed with `ghostcam-`
- **bits-ui** primitives in `components/ui/`, domain components alongside views
- Stores are exported object literals with `$state` fields — not class-based
