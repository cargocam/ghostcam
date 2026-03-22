# Ghostcam — Notifications

**Status:** Planned — not implemented in v1. In v1, event visibility is limited to the in-session UI alerts system (camera online/offline events within an active browser session).

---

## 1. Overview

This document will specify the notification system for Ghostcam — the mechanisms by which users are alerted to camera events when they are not actively watching the UI.

Three delivery mechanisms are planned:

| Mechanism | Audience | Variant |
|-----------|----------|---------|
| Webhooks | Integrators, self-hosters, Home Assistant etc. | Both |
| Email | End users | `server-multi` |
| Push notifications | Mobile app users | `server-multi`, gated on native mobile app |

None of these are implemented in v1. The in-session alerts sheet in the UI is the only notification surface at launch.

---

## 2. Known v1 Limitation

Events that occur while no browser session is open are not recorded or surfaced anywhere in v1. A camera going offline at 3am is invisible to the operator until they next open the UI. This is a known limitation and the primary motivation for this spec.

---

## 3. Planned Webhook Model

Webhooks are the highest priority delivery mechanism — they serve both `server-solo` and `server-multi`, require no mobile app, and enable integration with the broader smart home ecosystem.

**Planned API surface (reserved, returns 501 in v1):**

```
POST   /api/v1/webhooks              Register a webhook endpoint
GET    /api/v1/webhooks              List registered webhooks
DELETE /api/v1/webhooks/{id}         Remove a webhook
GET    /api/v1/webhooks/{id}/logs    Delivery attempt log
```

**Planned event types:**

| Event | Trigger |
|-------|---------|
| `camera.online` | Camera connects to server |
| `camera.offline` | Camera disconnects |
| `camera.storage_full` | Recording paused due to storage |
| `camera.update_succeeded` | Firmware update applied |
| `camera.update_failed` | Firmware update failed or rolled back |
| `camera.enrolled` | New camera enrolled |
| `camera.unregistered` | Camera unregistered |
| `event.motion` | Motion detected (gated on `events.md`) |
| `event.audio` | Audio threshold exceeded (gated on `events.md`) |

**Planned delivery semantics:** retry with exponential backoff, 3 attempts over 5 minutes, then mark failed. Delivery log queryable via API.

**Planned payload envelope:**
```json
{
  "event": "camera.offline",
  "ts": "2026-03-18T14:23:01.234Z",
  "device_id": "...",
  "display_name": "Back Door",
  "data": { }
}
```

**Planned signing:** HMAC-SHA256 of the payload body using a per-webhook secret, delivered in `X-Ghostcam-Signature` header. Receivers verify before processing.

---

## 4. Planned Email Model

Email notifications for `server-multi` users. Requires SMTP configuration or a transactional email provider (SendGrid, Postmark, Resend).

User-configurable per event type. Digest mode (batch events into a single email) vs immediate mode.

---

## 5. Planned Push Notification Model

Push notifications via APNs (iOS) and FCM (Android). Gated on a native mobile app — not applicable until the mobile app is developed. The browser-based UI in v1 does not support push notifications.

---

## 6. Dependencies

- `events.md` — motion and audio event types require the event detection pipeline
- `database.md` — webhook registrations require a new `webhooks` table and `webhook_deliveries` log table
- Native mobile app — push notifications are blocked on this
