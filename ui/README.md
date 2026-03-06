# Ghostcam Viewer

Svelte 5 SPA for viewing live camera feeds from the Ghostcam bridge over WebRTC.

![Ghostcam viewer with 4 simulated cameras](assets/screenshot.png)

## Setup

```bash
bun install
bun run dev       # Dev server at http://localhost:5173 (proxies /api → :3000)
bun run build     # Production build
bun run check     # Type-check
```

## Tech Stack

- **Svelte 5** with runes (`$state`, `$derived`, `$effect`)
- **Vite 6** dev server and build
- **Tailwind CSS 4** with OKLCH color tokens
- **bits-ui 2** headless component primitives
- **lucide-svelte** icons
- **Leaflet** map integration

## Features

- Multi-camera grid with auto-fit and 1+5 featured layouts
- Full-screen single-camera view (keyboard: F/M/S/P/Esc)
- Live telemetry display (CPU, memory, temperature, uptime, GPS)
- Camera group switching
- Inline camera renaming (persisted to localStorage)
- Picture-in-Picture and snapshot capture
- Map view with camera markers (dot/detailed/PiP modes)
- Dashboard view with aggregate stats and sparkline charts
- Connection alerts (disconnect/reconnect notifications)
- Dark/light/system theme
- Mobile responsive (sidebar + drawer nav)

## Architecture

### Connection Flow

1. Fetch group list from bridge HTTP API
2. Create `RTCPeerConnection` with recv-only transceivers (1 video + 1 audio per camera)
3. Create `"telemetry"` data channel
4. Generate SDP offer, POST to `/api/v1/watch/{group_id}`
5. Apply SDP answer, receive media tracks
6. `track_map` message maps SDP mids to device IDs
7. Poll WebRTC stats every 2s for bitrate/frame metrics

### Stores

| Store | File | Purpose |
|-------|------|---------|
| `transportStore` | `transport.svelte.ts` | WebRTC connection lifecycle, reconnect, stats polling |
| `cameraStore` | `cameras.svelte.ts` | Camera registry, streams, telemetry, selection |
| `groupStore` | `groups.svelte.ts` | Group list and active group |
| `settingsStore` | `settings.svelte.ts` | Theme, grid layout, view mode, sidebar (localStorage) |
| `alertStore` | `alerts.svelte.ts` | Disconnect/reconnect event log |
| `cameraConfigStore` | `cameraConfig.svelte.ts` | Per-camera display name overrides (localStorage) |
| `videoStatsStore` | `videoStats.svelte.ts` | Per-track WebRTC inbound-rtp stats |
| `thumbnailStore` | `thumbnails.svelte.ts` | Canvas-captured frame thumbnails (data URLs) |

Stores are exported object literals with `$state` fields and methods (not class-based).

### Key Files

| File | Purpose |
|------|---------|
| `signaling.ts` | HTTP client for SDP exchange + REST API |
| `webrtc.ts` | `RTCPeerConnection` lifecycle, track mapping, data channel setup |
| `data-channel.ts` | Routes incoming JSON messages to appropriate stores |

### Data Channel Messages (bridge → viewer)

| Type | Handler |
|------|---------|
| `cameras` | `cameraStore.setCameras()` |
| `camera_join` | `cameraStore.addCamera()` + alert + reconnect |
| `camera_leave` | `cameraStore.removeCamera()` + alert |
| `telemetry` | `cameraStore.updateTelemetry()` |
| `track_map` | `WebRtcSession.handleTrackMap()` |
| `renegotiate` | `transportStore.reconnect()` |

## Conventions

- **Svelte 5 runes only** — no legacy `$:` reactivity
- **Tailwind CSS 4** — OKLCH color tokens in `app.css`, `cn()` for class merging
- **localStorage** keys prefixed with `ghostcam-`
- **bits-ui** primitives in `components/ui/`, domain components alongside views
