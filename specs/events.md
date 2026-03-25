# Ghostcam — Camera Events

**Status:** Planned — not implemented in v1.

---

## 1. Overview

This document will specify the camera-side event detection pipeline, the event data model, and how events flow through the Ghostcam system to trigger notifications and appear in the UI timeline.

Events are discrete occurrences detected by the camera or server — motion, audio above a threshold, camera obstruction, connectivity loss — as distinct from continuous telemetry streams. In v1, camera-side event detection is not implemented. The notification system (see `notifications.md`) is gated on this spec.

---

## 2. Planned Event Types

| Event | Detection site | Notes |
|-------|---------------|-------|
| Motion detected | Camera (edge) | Requires frame analysis |
| Audio threshold exceeded | Camera (edge) | Passive monitoring of all audio tracks |
| Camera obstructed | Camera (edge) | Sudden loss of image detail / uniform frame |
| Recording paused | Camera | Storage full — already implemented via `storage_full` alert |
| Connectivity lost | Server | Camera QUIC disconnect exceeding a threshold duration |

---

## 3. Open Design Questions

**Where does detection run?**

The Pi Zero 2W is compute-constrained. Running motion detection on-device competes with H.264 encoding and fMP4 muxing. Options:

- **Edge inference on camera** — lowest latency, no server-side frame access required, limited by Pi Zero 2W compute. The Pi AI Camera (IMX500) would make this significantly more feasible but is not the v1 hardware.
- **Server-side frame analysis** — server receives frames over the existing WebRTC path and runs detection. Requires the server to decode H.264, which conflicts with the "server does not transcode or analyse media" design principle. Not recommended.
- **Hybrid** — camera runs lightweight motion detection (frame differencing, no ML), server handles more sophisticated analysis if/when a more capable camera module is supported.

Frame differencing for motion detection is feasible on the Pi Zero 2W and doesn't require ML — it's a natural v2 starting point.

**What does an event record contain?**

At minimum: `device_id`, `event_type`, `ts`, `duration_ms`. Richer events would carry a thumbnail (JPEG frame capture at detection time) and a `segment_id` reference pointing to the fMP4 segment containing the event, enabling direct timeline scrub to the event.

**How do events flow through the system?**

Proposed path:
- Camera sends a new `event` alert type on the Alerts stream (see `wire-protocol.md`)
- Server persists the event to the application database (`events` table)
- Server fans out to subscribed observers via the WebRTC data channel
- Server triggers the notification system (see `notifications.md`)

This would require a new `events` table in `database.md`, a new alert type in `wire-protocol.md`, and new data channel message types in `webrtc-client.md`.

**How do events appear in the UI?**

Events are natural overlays on the timeline scrubber — markers at the timestamp where the event occurred. Clicking a marker seeks to that position. This requires additions to `ui.md` §5.

---

## 4. Hardware Note

v1 hardware is the IMX219 on Pi Zero 2W. Future camera module support (including the IMX500 AI Camera with on-chip inference) is under consideration for v2. The event detection architecture should not assume ML inference capability on the camera until hardware is confirmed.

---

## 5. Dependencies

- `notifications.md` — events are the primary trigger for the notification system
- `wire-protocol.md` — new `event` alert type required
- `database.md` — new `events` table required
- `webrtc-client.md` — new data channel message type for real-time event delivery
- `ui.md` — timeline event markers
