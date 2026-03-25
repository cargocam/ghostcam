# Ghostcam — System Overview

**Status:** Draft

---

## 1. Introduction

Ghostcam is a security camera system in which cameras connect to a server over QUIC with mutual TLS and observers connect to the server over WebRTC. The server is a protocol translator — it forwards encoded frames from cameras to observers and manages the control plane between them. It does not transcode, mix, or store media.

This document provides a high-level map of the system: its components, their responsibilities, the data flows between them, and where to find the detailed specification for each area.

---

## 2. Spec Index

| Spec | Scope |
|------|-------|
| `wire-protocol.md` | QUIC channel layout, stream framing, alert and command message schemas, connection lifecycle, telemetry buffer upload |
| `auth.md` | Two-layer certificate model, QR enrollment flow, unregistration, revocation list |
| `ingest.md` | IngestSlot, QUIC accept loop, mTLS verification, broadcast fan-out, routing registry, live subscriber demand tracking |
| `playback.md` | fMP4 recording pipeline, HLS segment and manifest format, on-demand segment upload, server proxy, client player model |
| `camera-firmware.md` | Capture pipeline, adaptive bitrate, ring buffer, telemetry buffer, QUIC connection lifecycle, certificate storage |
| `webrtc-client.md` | PeerConnection lifecycle, signaling, data channel protocol, live and playback players, timeline |
| `telemetry.md` | Redis persistence, write path, REST query API, pagination, historic telemetry for scrubbing |
| `ui.md` | View modes (grid, focus), timeline scrubber behaviour, playback controls, audio focus model, noise detection |
| `database.md` | Application database schema, server-solo vs server-multi variants, user and camera data models, auth tokens |
| `pki.md` | CA hierarchy, instance CA bootstrap, camera trust model, enrollment JWT signing, server TLS |
| `deployment.md` | Server installation, camera provisioning, health checks, audit logging, backup/restore, OTA operator guide |
| `notifications.md` | Planned — webhooks, email, push notifications (post-MVP) |
| `events.md` | Planned — camera-side event detection, motion, audio (post-MVP) |
| `multi-server.md` | Planned — horizontal scaling architecture (post-MVP) |

---

## 3. Components

### Camera (Pi Zero 2W)

The camera is a Raspberry Pi Zero 2W running Ghostcam firmware. It captures H.264 video via `rpicam-vid` and Opus audio via `cpal`, fans both streams to a live QUIC path and a local fMP4 recording pipeline simultaneously. It maintains a rolling ring buffer of fMP4 segments on a dedicated data partition — filling available storage with footage, evicting oldest segments as needed — generates an HLS manifest reflecting available footage, and streams live sensor telemetry (GPS if available, signal strength, temperature, frame stats) to the server as QUIC datagrams.

The camera maintains an on-disk telemetry buffer for datagrams generated while the QUIC connection is unavailable. Buffered entries are uploaded to the server on reconnect via a dedicated QUIC stream, then cleared from disk.

The camera connects outbound to the server over QUIC with mutual TLS, presenting a device identity certificate (issued at flash time, permanent) and a user association certificate (issued on enrollment, revocable). If the camera has no user association certificate on startup, it enters registration mode and waits for the user to complete the QR enrollment flow.

### Server

The server is a Rust service using Quinn (QUIC) for camera ingest and str0m (WebRTC) for observer egress. It receives encoded frames from cameras and fans them out to subscribed observers. In-memory state (IngestSlots, EgressHandles, routing registry, manifest cache) is soft — reconstructible from camera and observer reconnections. Durable state lives in two stores: the application database (camera enrollment records, user credentials, API tokens) and Redis (telemetry history, segment metadata, certificate revocation list).

The server's core abstractions are:

- **IngestSlot** — one per connected camera. Owns the QUIC read loops, demuxes channels, holds `broadcast::Sender`s for video, audio, and telemetry fan-out.
- **EgressHandle** — one per active observer×camera pair. Subscribes to the IngestSlot's broadcast channels and drives the WebRTC send loop.
- **Routing registry** — maps `user_id → [IngestSlot]` and `user_id → [EgressHandle]`.

### Application Database

The application database holds structured, relational state: camera enrollment records, user credentials, session tokens, API tokens, and enrollment tokens. In `server-solo` this is SQLite; in `server-multi` it is Postgres. See `database.md` for the full data model.

### Redis

Redis holds durable state that benefits from its stream and set data structures: telemetry history (one Redis Stream per camera, 72-hour TTL), segment metadata (segment index used for playback window queries and upload deduplication), and the certificate revocation list. The server holds the latest HLS manifest per camera in memory only — it is not persisted to Redis or the application database.

### Client (Svelte / TypeScript)

The client is a Svelte SPA. It holds one `RTCPeerConnection` per camera it is watching, an SSE connection for server-to-client push events, and for playback an hls.js (or native HLS) player. Each PeerConnection carries two data channels: an unreliable unordered channel for high-frequency telemetry (server→client, MessagePack) and a reliable ordered channel for client commands such as `client_mode` (client→server, JSON). Live video and audio arrive over WebRTC; historic footage is fetched via HLS; telemetry arrives over the WebRTC data channel (live) and REST (historic).

---

## 4. System Topology

```
+-------------------------------------------------------------------+
|                            Camera                                 |
|                         (Pi Zero 2W)                              |
|                                                                   |
|  rpicam-vid --> H.264 --+-> QUIC Video stream (live)              |
|                         +-> fMP4 muxer --> .m4s segments          |
|                                       --> playlist.m3u8           |
|  cpal --> PCM --> Opus -+-> QUIC Audio stream (live)              |
|                         +-> fMP4 muxer                            |
|                                                                   |
|  gpsd (optional) ---------> QUIC Telemetry datagrams              |
|  sensors -----------------> QUIC Telemetry datagrams              |
|  Telemetry buffer --------> QUIC Upload stream (on reconnect)     |
|  Commands stream <--------- Server                                |
|  Upload streams ----------> Server (on demand / on reconnect)     |
+------------------------------+------------------------------------+
                               | QUIC / mTLS
                               v
+-------------------------------------------------------------------+
|                            Server                                 |
|                                                                   |
|  +------------------------------------------------------------+   |
|  | IngestSlot (per camera)                                    |   |
|  |  QUIC read loops                                           |   |
|  |  broadcast::Sender<VideoFrame>      (capacity: 512)        |   |
|  |  broadcast::Sender<AudioFrame>      (capacity: 512)        |   |
|  |  broadcast::Sender<TelemetryFrame>  (capacity: 512)        |   |
|  |  manifest: String (in-memory)                              |   |
|  |  in-flight segment map                                     |   |
|  +---------------------------+--------------------------------+   |
|                              | broadcast fan-out                  |
|          +-------------------+-------------------+               |
|          v                   v                   v               |
|  +--------------+   +--------------+   +--------------+          |
|  |EgressHandle A|   |EgressHandle B|   |EgressHandle C|          |
|  |str0m PC      |   |str0m PC      |   |str0m PC      |          |
|  +--------------+   +--------------+   +--------------+          |
|                                                                   |
|  +------------------------------------------------------------+   |
|  | Application Database (SQLite / Postgres)                   |   |
|  |  cameras, users, sessions, api_tokens, enrollment_tokens  |   |
|  +------------------------------------------------------------+   |
|                                                                   |
|  +------------------------------------------------------------+   |
|  | Redis                                                      |   |
|  |  telemetry:{device_id} (Stream, 72h TTL)                  |   |
|  |  segment metadata (72h TTL)                               |   |
|  |  revoked_certs (Set<serial_number>)                       |   |
|  +------------------------------------------------------------+   |
+----------+--------------------+--------------------+--------------+
           | WebRTC             | WebRTC             | WebRTC
           | SSE / REST         | SSE / REST         | SSE / REST
           v                    v                    v
        Observer A          Observer B          Observer C
```

---

## 5. Live Streaming Data Flow

The live path carries video, audio, and telemetry from camera to observer with minimal server involvement.

```
Camera                      Server                       Observer
  |                           |                            |
  |-- H.264 frames ---------->|                            |
  |   (QUIC Video stream)     |-- RTP video -------------->|
  |                           |   (WebRTC)                 |
  |-- Opus frames ----------->|                            |
  |   (QUIC Audio stream)     |-- RTP audio -------------->|
  |                           |   (WebRTC)                 |
  |-- telemetry datagrams --->|-- broadcast to egress ---->|
  |   (QUIC datagrams)        |   (WebRTC telemetry chan)  |
  |                           |-- XADD Redis               |
  |                           |   (persist telemetry)      |
```

The server reads one frame, broadcasts it to N egress handles, and writes telemetry to Redis. Ingest is O(cameras); fan-out cost is O(cameras × observers) only at the send layer.

---

## 6. Playback Data Flow

Historic footage is delivered via HLS. The camera uploads fMP4 segments on demand over the existing QUIC connection. The server proxies them to the client with request coalescing.

```
Client                      Server                       Camera
  |                           |                            |
  |-- GET playlist.m3u8 ----->|                            |
  |<-- M3U8 manifest ---------|  (served from memory)      |
  |                           |                            |
  |-- GET {segment_id}.m4s -->|                            |
  |                           |-- upload_segment cmd ----->|
  |                           |<-- QUIC upload stream -----|
  |                           |    (raw .m4s bytes)        |
  |<-- .m4s segment -----------|  (proxied, buffered 60s)  |
  |                           |                            |
  |   hls.js stitches         |                            |
  |   segments, plays         |                            |
```

Historic telemetry for the scrub window is fetched separately:

```
Client                      Server                      Redis
  |                           |                            |
  |-- GET /telemetry?from=&to=|                            |
  |                           |-- XRANGE ----------------->|
  |                           |<-- entries -----------------|
  |<-- JSON entries -----------|                            |
```

---

## 7. Enrollment Flow

A camera begins in registration mode when it has no user association certificate. Enrollment associates it with a user account using a proximity-gated QR code system.

```
User App                    Server                       Camera
  |                           |                            |
  |-- POST /cameras/enroll -->|                            |
  |                           |-- generate enrollment token|
  |                           |-- encode as QR             |
  |<-- QR code ---------------|                            |
  |                           |                            |
  |  (user scans QR,          |                            |
  |   camera reads token)     |                            |
  |                           |<-- QUIC connect -----------|
  |                           |    (device cert + token)   |
  |                           |-- verify device cert       |
  |                           |-- verify token             |
  |                           |-- open Commands stream     |
  |                           |<-- csr alert --------------| (camera generates key pair)
  |                           |-- sign CSR                 |
  |                           |-- cert_refresh cmd ------->|
  |                           |                  store cert|
  |                           |<-- ack --------------------|
  |                           |-- mark enrolled in DB      |
  |<-- 200 OK -----------------|                            |
```

After enrollment the camera presents both certificates on all subsequent connections.

---

## 8. Implicit Live Stream Demand

The server manages camera live streaming implicitly — no client action is required to start or stop the camera streaming. The server tracks live video and audio subscriber counts per camera via `client_mode` messages on the reliable commands data channel, and issues `start_video` / `stop_video` (and audio equivalents) as counts cross zero.

```
Client A joins (mode: live)
  -> video_subscribers: 0 -> 1
  -> server sends start_video to camera
  -> camera begins writing to Video stream

Client B joins (mode: map)
  -> video_subscribers unchanged: 1
  -> no command sent

Client A switches to playback
  -> video_subscribers: 1 -> 0
  -> server sends stop_video to camera
  -> camera stops writing to Video stream
  -> Client B (map mode) unaffected
```

---

## 9. Key Design Decisions

**Server is not an SFU.** The server does not transcode, mix, or re-encode media. It reads encoded frames from the camera and writes them to observer RTP tracks unchanged. All encoding decisions are made by the camera.

**Camera is the authoritative footage store.** Raw fMP4 segments live on the camera. The server holds only segment metadata in Redis and the latest HLS manifest in memory. Footage is delivered to clients on demand via QUIC upload streams — the server never stores segment data beyond a 60-second in-memory buffer.

**Server in-memory state is soft.** All in-memory server state — IngestSlots, EgressHandles, routing registry, manifest cache — is reconstructible from camera and observer reconnections. The server can restart without media data loss. Durable state lives in the application database (enrollment records, credentials) and Redis (telemetry history, segment metadata, revocation list).

**The server is single-instance.** All in-memory state is process-local. Running multiple server instances behind a load balancer will not work correctly: cameras and observers routed to different instances cannot communicate, SSE events will not propagate across instances, and `start_video`/`stop_video` demand tracking will be incorrect. Horizontal scaling is a planned future capability tracked in `multi-server.md`.

**Live and playback are separate transport paths.** Live video and audio travel over WebRTC RTP tracks. Historic footage travels over HLS via HTTP. Both are rendered in a unified single-viewport player on the client — the transport difference is invisible to the user.

**Commands are camera-level, not client-level.** Seek, start/stop streaming, and other commands affect the camera as a whole. All observers watching a camera see the result of any command.

**Telemetry is threshold-driven with a heartbeat.** The camera polls sensors every 2 seconds and transmits immediately when any field exceeds its per-field threshold (CPU: 5%, temp: 1°C, memory: 5 MB, GPS: ~11 metres). A full heartbeat is sent every 30 seconds regardless. When offline, datagrams are written to an on-disk buffer with deduplication to avoid storing redundant heartbeat runs. On reconnect the buffer is uploaded and cleared. GPS position during offline periods is preserved in the on-disk record.

**Enrollment is proximity-gated.** Device enrollment uses a QR code system — the server generates a short-lived enrollment token, encodes it as a QR code, and the camera scans it. The camera generates a key pair locally, sends a CSR to the server, and receives only the signed certificate in return. Private keys never leave the camera. Physical proximity is required to enroll a device, preventing remote enrollment attacks.

**User association certificates are permanent.** They do not expire. Revocation via the server-internal Redis list is the sole mechanism for invalidating a certificate. This prevents lockout of cameras that are unused for extended periods.

**Revocation is server-internal.** Certificate revocation uses a server-internal list stored in Redis, checked at every QUIC connection. No external CRL or OCSP infrastructure is required.

---

## 10. Technology Stack

| Layer | Technology |
|-------|------------|
| Camera capture | `rpicam-vid` (H.264), `cpal` (Opus), `gpsd` (GPS, optional) |
| Camera recording | fMP4 muxer, HLS manifest |
| Camera transport | Quinn (QUIC), mutual TLS via rustls |
| Server ingest | Quinn (QUIC), Tokio, Axum |
| Server egress | str0m (WebRTC) |
| Server TLS | rustls + aws-lc-rs (FIPS 140-3) |
| Telemetry encoding | MessagePack (rmp-serde) |
| Application database (`server-solo`) | SQLite via sqlx |
| Application database (`server-multi`) | Postgres via sqlx |
| Telemetry / revocation persistence | Redis Streams + Sets (72h TTL) |
| Client framework | Svelte 5, TypeScript |
| Client live player | WebRTC (browser native) |
| Client playback player | hls.js / native HLS |

---

## 11. Open Questions Summary

| Question | Spec(s) |
|----------|---------|
| QUIC application error code for unenrolled device | `wire-protocol.md`, `ingest.md` |
| `network_config` — ACK and rollback for bad WiFi credentials | `wire-protocol.md`, `camera-firmware.md` |
| Bandwidth contention between live streaming and segment upload | `playback.md`, `ingest.md` |
| `server-solo` first-run UX — stdout password vs. `--initial-password` flag | `database.md` |
| Email verification in `server-multi` — required before enrollment? Delivery mechanism? | `database.md` |
| Password reset flow in `server-multi` | `database.md` |
| Session token transport — `HttpOnly` cookie vs. bearer token | `database.md` |
| Rate limiting on auth endpoints | `database.md` |
| Enrollment JWT — camera-side signature verification (planned hardening) | `auth.md`, `pki.md` |
| `server-multi` intermediate CA rotation procedure | `pki.md` |
| `server.pin` rotation on `server-solo` after key compromise | `pki.md` |
