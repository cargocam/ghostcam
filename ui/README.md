# Ghostcam Viewer

Svelte 5 SPA for the Ghostcam surveillance system. Provides hybrid WebRTC/HLS live viewing (sub-second latency via WebRTC with automatic HLS fallback), HLS playback of recorded footage with a timeline scrubber, a GPS map, telemetry dashboards, and camera management.

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
- **@ffmpeg/ffmpeg 0.11.6** — client-side MP4 assembly via ffmpeg.wasm (lazy-loaded)

## Features

- Password-protected login (session cookie)
- Multi-camera grid — auto-fit and 1+5 featured layouts
- Hybrid WebRTC/HLS live viewing — WebRTC for sub-second latency, automatic HLS fallback
- Badge shows "LIVE" (WebRTC active, green) or "DELAYED" (HLS fallback, orange)
- Timeline scrubber — global playhead for navigating recorded footage and historical telemetry
- Telemetry history — fetch and display CPU, memory, temperature, GPS at any past point in time
- GPS map with camera markers and playback trail overlay
- Dashboard view with aggregate stats and sparkline charts
- Camera online/offline status, display name overrides (localStorage)
- Connection alerts (disconnect/reconnect notifications)
- Clip mode — timeline range selection with drag handles, loop playback preview
- Download clip as MP4 (ffmpeg.wasm remux), or export telemetry as CSV/JSON
- Dark/light/system theme, mobile responsive (sidebar + drawer nav)

## Architecture

### Transport Layer

Camera events and state arrive via **Server-Sent Events** (`/events`). Live video uses a **hybrid WebRTC/HLS** approach: the `LivePlayer` component renders both an `HlsPlayer` (always running) and a `WebRtcPlayer` (live mode only). When WebRTC connects (via WHEP: `POST /api/v1/whep/:deviceID`), it overlays the HLS player for sub-second latency. On WebRTC failure, the already-buffered HLS stream shows instantly. Historical telemetry is fetched on demand via the **REST API** (`/api/v1/telemetry/:id`).

### Stores

| Store | File | Purpose |
|-------|------|---------|
| `transportStore` | `transport.svelte.ts` | SSE connection, authentication state |
| `cameraStore` | `cameras.svelte.ts` | Camera registry, live streams, telemetry, online status, selection |
| `scrubberStore` | `scrubber.svelte.ts` | Timeline mode (`live`/`playback`), playhead time, mode change callbacks |
| `groupStore` | `groups.svelte.ts` | Group list and active group |
| `settingsStore` | `settings.svelte.ts` | Theme, grid layout, view mode, mute state (localStorage) |
| `alertStore` | `alerts.svelte.ts` | Disconnect/reconnect event log |
| `clipStore` | `clip.svelte.ts` | Clip mode state: range, phase, progress, seekRevision |
| `cameraConfigStore` | `cameraConfig.svelte.ts` | Per-camera display name overrides (localStorage) |
| `videoStatsStore` | `videoStats.svelte.ts` | Per-track inbound-rtp stats |
| `thumbnailStore` | `thumbnails.svelte.ts` | Canvas-captured frame thumbnails (data URLs) |

### Key Library Files

| File | Purpose |
|------|---------|
| `auth.ts` | Login, logout, session check |
| `sse.ts` | SSE client — parses events, drives cameraStore and transportStore |
| `signaling.ts` | API client helpers — `listCameras`, `fetchCoverage`, billing, events, clips, telemetry |
| `ffmpeg.ts` | Lazy-loaded ffmpeg.wasm 0.11.x — `concatSegments()` downloads TS segments, remuxes to MP4 |
| `telemetry-history.ts` | Fetch telemetry time ranges from API with in-memory cache; `nearestTelemetryEntryWithin` |

### Views

| View | Description |
|------|-------------|
| `LiveView` | Camera grid — LivePlayer per card (hybrid WebRTC+HLS). Online cameras sorted first. |
| `CameraView` | Single fullscreen camera — keyboard shortcuts F/M/S/P/Esc |
| `MapView` | Leaflet map with camera markers at live or historical GPS positions; playback trail overlay |
| `DashboardView` | Aggregate telemetry — stats panels and sparklines, historical data on scrub |

### WebRTC Components

| Component | Description |
|-----------|-------------|
| `WebRtcPlayer.svelte` | WHEP client — creates `RTCPeerConnection`, POSTs SDP offer to `/api/v1/whep/:id`, receives answer. Retries 3 times then gives up. |
| `LivePlayer.svelte` | Orchestrator — always renders `HlsPlayer`, overlays `WebRtcPlayer` in live mode. Shows WebRTC when connected, falls back to HLS instantly. |

No TURN server or STUN configuration needed — the server is ICE-lite with a known public IP (`GHOSTCAM_PUBLIC_IP`). If a viewer's network blocks UDP, WebRTC fails silently and HLS takes over.

## E2E Testing

End-to-end tests use [Playwright](https://playwright.dev/) with Chromium. All server interactions are mocked via `page.route()` — no running backend required.

```bash
# Install browser binaries (one-time)
bunx playwright install chromium

# Run tests
bun run test:e2e

# Run tests with interactive UI
bun run test:e2e:ui
```

### Test Structure

Tests live in `e2e/` and shared helpers (API mocking, mock data) are in `e2e/helpers.ts`.

| File | Coverage |
|------|----------|
| `login.spec.ts` | Login form rendering, validation, wrong password error, successful login, connection failure |
| `camera-grid.spec.ts` | Camera card display, online/offline badges, selection (ring highlight), double-click to camera view, empty state |
| `settings.spec.ts` | Theme persistence (dark/light), grid layout persistence, mute state persistence in localStorage |
| `sse-events.spec.ts` | Initial camera count, new camera appearing in grid, offline status update, camera card rendering |

### Writing New Tests

- Use `mockAuthenticatedSession(page)` from `helpers.ts` to set up all route intercepts before `page.goto('/')`.
- Camera names appear in both the sidebar and the camera grid — use `.first()` or scope locators to `page.locator('main')` to avoid strict mode violations.
- Camera card status badges (LIVE/OFF) are `<span class="uppercase">` elements inside `main button.aspect-video`.
- The Playwright config (`playwright.config.ts`) auto-starts the Vite dev server.

## Conventions

- **Svelte 5 runes only** — no legacy `$:` reactivity
- **Tailwind CSS 4** — OKLCH tokens in `app.css`, `cn()` for class merging
- **localStorage** keys prefixed with `ghostcam-`
- **bits-ui** primitives in `components/ui/`, domain components alongside views
- Stores are exported object literals with `$state` fields — not class-based
