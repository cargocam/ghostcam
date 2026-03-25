# Ghostcam — UI Specification

**Status:** Draft

---

## 1. Overview

This document specifies the client-side view modes, navigation structure, timeline scrubber behaviour, playback controls, audio focus model, camera tile states, and supporting UI components for the Ghostcam observer UI.

The WebRTC session model and data channel protocol are specified in `webrtc-client.md`. The HLS playback mechanism and player architecture are specified in `playback.md`. Telemetry data schema is specified in `telemetry.md`.

The client is a Svelte 5 TypeScript SPA using Tailwind CSS and bits-ui primitives.

---

## 2. Application Shell

### 2.1 Layout

The application shell consists of:

- **Header** — top bar with branding, view mode switcher, context controls, connection status, alerts button, and settings button
- **Sidebar** — persistent left panel on desktop containing the camera list and telemetry panel for the selected camera
- **Main content area** — renders the active view

On mobile the sidebar collapses and is accessible via a slide-in drawer from the header menu button.

### 2.2 Header

The header contains:

- **Branding** — Ghostcam wordmark
- **View mode switcher** — toggles between Live, Map, and Dashboard views
- **Context controls** — view-specific toggles that appear contextually:
  - Live view: grid layout toggle (auto-fit grid / focus layout)
  - Map view: marker mode toggle (dot / detailed / PiP)
- **Connection status** — shows connected/reconnecting/disconnected state; displays reconnect attempt count when reconnecting
- **Alerts button** — bell icon with unread count badge; opens the alerts sheet
- **Settings button** — opens the settings sheet

### 2.3 Sidebar

The sidebar is visible on desktop. It contains:

- **Camera list** — all cameras, each showing name and online/offline indicator. Clicking a camera selects it and updates the telemetry panel.
- **Telemetry panel** — live telemetry for the selected camera: CPU usage with sparkline, memory usage with sparkline, temperature, and uptime. Displays a prompt to select a camera when none is selected.

### 2.4 Mobile nav drawer

On mobile a hamburger button in the header opens a slide-in sheet containing the same camera list and telemetry panel as the sidebar.

---

## 3. View Modes

The client supports three top-level view modes. Switching view modes does not affect any camera's transport or playback state.

| Mode | Description |
|------|-------------|
| Live | Camera grid with unified or per-camera playback controls |
| Map | Leaflet map showing GPS-positioned camera markers |
| Dashboard | Aggregate stats and per-camera metrics table |

### 3.1 Live view

Live view displays all connected cameras in a tile grid. It is the primary view for monitoring.

Two layouts are available, toggled from the header context controls:

- **Auto-fit grid** — all camera tiles arranged in a uniform responsive grid. Tile count and size adapt to viewport dimensions.
- **Focus layout** — one camera tile is enlarged and displayed prominently; remaining cameras are displayed smaller around it (see §3.2).

The active layout is persisted to localStorage.

A single unified timeline scrubber is displayed at the bottom of the live view. In auto-fit grid mode it controls all visible cameras in unison. In focus layout it controls only the focused camera (see §5 for scrubber behaviour).

### 3.2 Focus layout

In focus layout one camera is designated the focused camera. Its tile is enlarged; all other tiles are displayed smaller around it.

The scrubber controls only the focused camera. Scrubbing puts the focused camera into playback; all other cameras remain live regardless.

The focused camera can be changed by clicking any non-focused tile:
- The previously focused camera returns to live mode if it was in playback
- The newly focused camera becomes the scrubber target
- All non-focused cameras remain live

### 3.3 Map view

Map view displays a Leaflet map with markers at each camera's GPS position. Cameras without a GPS fix are not shown on the map.

#### Marker modes

Three marker display modes are available, toggled from the header:

| Mode | Description |
|------|-------------|
| Dot | Small coloured dot indicating online/offline status |
| Detailed | Marker with camera name, status, and key telemetry values |
| PiP | Marker with a live video thumbnail captured from the camera's WebRTC track |

#### Map tracking

The map supports three tracking states:

- **Track all** — map auto-fits to show all GPS-positioned cameras; updates as cameras move
- **Track single** — map pans to follow a selected camera
- **Manual** — user has taken manual control; no auto-pan

Tracking disengages automatically when the user drags or zooms the map. Clicking a camera marker enters single-camera tracking for that camera. A focus toggle button re-engages track-all mode.

#### Tile layers

The map uses CARTO tile layers. The tile layer switches between light and dark variants to match the active application theme.

### 3.4 Dashboard view

Dashboard view provides a system-level overview and per-camera metrics table.

#### Stat cards

Four summary cards:

| Card | Content |
|------|---------|
| Cameras online | Online count and total count |
| Total bandwidth | Aggregate inbound bitrate across all cameras with sparkline |
| Frames decoded | Total decoded frame count and dropped frame count |
| Connection uptime | Time since current session connected and connection state |

#### Status cards

Three status cards:

| Card | Content |
|------|---------|
| Connection | Connected/disconnected state and any error message |
| Stream health | Frame drop rate percentage with qualitative indicator (Excellent / Good / Degraded) |
| Alerts | Unread alert count and total alert count |

#### Per-camera table

A scrollable table listing all cameras with columns:

| Column | Description |
|--------|-------------|
| Camera | Display name |
| Status | Online / offline indicator |
| Resolution | Video resolution (e.g. 640×480) |
| Codec | Video codec reported by WebRTC stats |
| Bitrate | Current inbound bitrate |
| Dropped | Dropped frame count |
| CPU | Camera CPU usage (from telemetry) |
| Memory | Camera memory usage (from telemetry) |
| Temp | Camera SoC temperature (from telemetry) |

---

## 4. Camera Tile

Each camera is represented by a tile. A tile renders one of the following states:

| State | Condition | Display |
|-------|-----------|---------|
| Live | Camera online, `client_mode: "live"`, frames arriving | Live WebRTC video |
| Playback | `client_mode: "playback"`, HLS segment loaded | HLS video with playback controls |
| Loading | Playback requested, segment upload in progress | Spinner overlay |
| No footage | Playback requested, segment returned 404 | "No footage" message with nearest available window indicated |
| No signal | Camera online, `client_mode: "live"`, no frames arriving | "No signal" message |
| Storage full | Camera sent `storage_full` alert | "Recording paused — storage full" warning overlay; live streaming unaffected |
| Offline | SSE `camera_offline` event received | "Offline" message |

Tile layout and dimensions are determined by the active view mode and layout. State transitions do not affect layout — no tiles are added or removed on state change.

### 4.1 Tile overlay

Each tile displays a non-blocking overlay showing:

- Camera display name (editable inline — see §4.2)
- Audio focus indicator when this tile's audio is active (see §7)
- WebRTC debug stats when debug mode is enabled: bitrate, frame rate, resolution, codec, packet loss (see §8.2)

The overlay is rendered on top of the video.

### 4.2 Camera renaming

Camera display names can be edited inline by clicking the name in the tile overlay or in the sidebar camera list. The authoritative store for display names is the `cameras.display_name` column in the application database — edits are persisted via `PATCH /api/v1/cameras/{device_id}` and survive browser profile changes and cross-device sessions. The client updates its local state optimistically on edit and rolls back on server error. Display names do not affect the `device_id` used by the server for routing and storage.

### 4.3 Per-tile actions

Each tile exposes the following actions (via overlay buttons or context menu):

| Action | Description |
|--------|-------------|
| Picture-in-Picture | Opens the tile's video in the browser's native PiP window |
| Snapshot | Captures the current frame as a PNG download |

---

## 5. Timeline Scrubber

### 5.1 Structure

The scrubber is a horizontal timeline displaying the available footage window for the camera(s) it controls. It consists of:

- **Available window** — the range of timestamps for which footage exists, derived from the HLS manifest
- **Playhead** — current position; advances in real time in live mode
- **Go Live button** — appears in playback mode; returns to live on click
- **Timestamp label** — displays the current playhead position
- **Per-camera coverage indicators** — in auto-fit grid mode, indicators show which cameras have footage at the current playhead position

### 5.2 Live mode behaviour

In live mode the playhead sits at the trailing edge of the available window and advances in real time. The scrubber cannot be dragged in live mode. The Go Live button is not shown.

### 5.3 Entering playback

When the user clicks or drags to any position on the timeline, the affected camera(s) enter playback mode:

1. `client_mode: "playback"` is sent on the commands data channel for each affected camera
2. The HLS manifest is fetched (or refreshed if already loaded)
3. The HLS player seeks to the target timestamp
4. The tile transitions to the playback or loading state as appropriate
5. The Go Live button appears at the trailing edge of the scrubber

### 5.4 Returning to live

Clicking the Go Live button:

1. Sends `client_mode: "live"` on the commands data channel for each affected camera
2. The live WebRTC player becomes visible; the HLS player is hidden and destroyed
3. The playhead returns to the trailing edge and resumes real-time advance
4. The Go Live button is hidden

### 5.5 Scrub outside available window

If the user scrubs to a position outside the available window for a camera, the tile enters the no-footage state. The scrubber updates to reflect the actual available window. The user can scrub to a position within the available window to resume playback.

### 5.6 Auto-fit grid scrubber

In auto-fit grid mode the unified scrubber displays the **union** of all cameras' available windows — any point in time that any camera has footage for is scrubbable. Per-camera coverage indicators on the scrubber show which cameras have footage at the current playhead position. Cameras without footage at the current position render the no-footage tile state.

### 5.7 Focus layout scrubber

In focus layout the scrubber controls only the focused camera. Its available window reflects only that camera's manifest. Non-focused cameras remain live and are unaffected by scrubbing.

---

## 6. Playback Controls

Playback controls are displayed on tiles in playback mode and on the scrubber. All controls operate directly on the hls.js instance or native HLS `<video>` element — no server communication is required.

| Control | Action |
|---------|--------|
| Play | `video.play()` |
| Pause | `video.pause()` |
| Seek | `video.currentTime = target`; scrubber playhead updates to match |
| Go Live | Returns to live mode (see §5.4) |

In auto-fit grid mode, play and pause on the unified scrubber apply to all cameras currently in playback simultaneously.

---

## 7. Audio Focus

### 7.1 Model

At most one camera tile has active (unmuted) audio at any time. All tiles are muted by default on session start.

The user activates a tile's audio by clicking on it. This is called focusing the tile for audio. The focused tile's audio becomes active; any previously focused tile is muted.

Clicking an already-focused tile unfocuses it — its audio is muted and no tile has active audio.

This model applies in all view modes. In focus layout the audio-focused tile and the enlarged (layout-focused) tile are independent — the user may have audio active on any tile regardless of which is enlarged.

### 7.2 Audio focus indicator

The tile with active audio displays a visual indicator (e.g. speaker icon or highlighted border) to make the audio focus state unambiguous at a glance.

### 7.3 Planned enhancement: noise detection

A future enhancement will passively monitor audio levels across all camera tracks (including muted ones) and visually highlight tiles where noise is detected above a hardcoded client-side threshold. This allows the user to identify cameras with audio activity without manually cycling through them. The threshold value will be determined during implementation. Implementation details are deferred to a future revision of this document.

---

## 8. Supporting UI Components

### 8.1 Alerts system

The alerts system tracks camera connect and disconnect events for the current session.

- **Alert badge** — displayed on the header alerts button; shows the count of unread alerts
- **Alerts sheet** — slide-in panel listing all alerts for the session in reverse chronological order, with camera name, event type (connected/disconnected/storage full), and timestamp. Accessible via the header alerts button.
- **Dashboard alert card** — shows unread and total alert counts (see §3.4)

Alerts are in-memory only — they are not persisted across sessions. Events that occur while no browser session is open are not recorded anywhere in v1. This is a known limitation; a persistent notification system (webhooks, email, push) is planned post-MVP. See `notifications.md`.

### 8.2 Debug overlay

A toggleable per-tile WebRTC stats overlay showing:

- Inbound bitrate (kbps)
- Frame rate (fps)
- Resolution (width × height)
- Codec
- Packet loss

Enabled and disabled via the settings sheet. The setting is persisted to localStorage.

### 8.3 Settings sheet

A slide-in settings panel accessible from the header. Contains:

- **Theme** — light / dark / system (follows OS preference). Persisted to localStorage.
- **Debug overlay** — toggle WebRTC stats overlay on all tiles. Persisted to localStorage.
- **Connection status** — read-only display of current WebRTC connection state and any error message.

### 8.4 Telemetry panel and sparklines

The telemetry panel is displayed in the sidebar and mobile drawer for the selected camera. It shows:

- **CPU usage** — current percentage with a 60-sample sparkline chart
- **Memory usage** — current MB value with a 60-sample sparkline chart
- **Temperature** — SoC temperature in °C with warning and critical colour thresholds
- **Uptime** — formatted as days/hours/minutes

The sparkline is a reusable SVG component that renders a filled area chart from an array of numeric samples. It is also used on the dashboard for the aggregate bandwidth card.

### 8.5 Thumbnail store

The client maintains a per-camera store of the most recently captured video frame as a data URL. Thumbnails are captured from the live `<video>` element via a canvas draw. They are used as the live video image in PiP map markers (marker mode: PiP). Thumbnails are in-memory only and are not persisted.

---

## 9. Open Questions

| Question | Notes |
|----------|-------|
| Camera renaming optimistic update rollback UX | §4.2 specifies optimistic update with rollback on server error. The error state UX — what the user sees if the rename fails — is not specified. |
