# Ghostcam — Playback

**Status:** Draft

---

## 1. Overview

This document specifies the footage recording, storage, and playback pipeline for Ghostcam cameras. It covers the camera-side capture and muxing pipeline, the HLS segment and manifest format, the upload mechanism for on-demand segment delivery, the server-side proxy and request coalescing, and the client-side player model.

Live video and audio are handled separately and are specified in `wire-protocol.md` and `webrtc-client.md`. Historic telemetry queries are specified in `telemetry.md`. UI behaviour for playback controls across view modes is specified in `ui.md`.

---

## 2. Camera Recording Pipeline

The camera runs two parallel consumers from a single capture source, ensuring the hardware is accessed once regardless of how many downstream consumers exist.

```
rpicam-vid -> H.264 frames -+-> QUIC Video stream (live)
                            +-> fMP4 muxer
                                     ^
cpal -> PCM -> Opus encoder -+-> QUIC Audio stream (live)
                             +-> fMP4 muxer

fMP4 muxer -> init.mp4          (once per capture session)
           -> {segment_id}.m4s  (every ~10 seconds)
           -> playlist.m3u8     (updated per segment and per eviction)
```

### 2.1 Segment format

Segments are fragmented MP4 (fMP4) containing muxed H.264 video and Opus audio. fMP4 is chosen because it carries Opus natively, is supported by HLS version 7+, and is handled by hls.js without transcoding.

Each segment is GOP-aligned — the muxer forces a keyframe at every segment boundary, making every segment independently decodable after the init segment.

Target segment duration is 10 seconds. The muxer finalises a segment every 10 seconds and immediately begins the next.

### 2.2 Init segment

The fMP4 init segment (`init.mp4`) contains codec parameters required by the client before any `.m4s` segment can be decoded. It is generated once when capture starts. If capture parameters change (resolution, codec profile) a new init segment is generated.

The init segment is not stored alongside recording segments — it is delivered to the server on demand via the upload mechanism (see §4.2).

### 2.3 Ring buffer

The camera maintains a rolling ring buffer of `.m4s` segments on the data partition (`/var/ghostcam/segments/`). The buffer fills all available space on the partition — there is no artificial time-based cap. When a new segment would exceed available space, the oldest segment is evicted to make room.

On eviction the camera:
1. Deletes the `.m4s` file from local storage
2. Updates `playlist.m3u8` to remove the evicted segment
3. Sends a `segment_evicted` alert to the server
4. Opens a manifest push stream to the server and writes the updated manifest

### 2.4 Manifest format

The camera maintains a sliding window HLS manifest covering all segments currently on disk. The manifest uses HLS version 7 with fMP4 segments.

```m3u8
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:10
#EXT-X-MAP:URI="init.mp4"
#EXTINF:10.0,
pi-zero-a1b2:1700000000000.m4s
#EXTINF:10.0,
pi-zero-a1b2:1700000010000.m4s
#EXTINF:10.0,
pi-zero-a1b2:1700000020000.m4s
```

There is no `#EXT-X-ENDLIST` tag — the manifest is open-ended, reflecting the rolling nature of the buffer. The `#EXT-X-MAP` URI is a relative path resolved by the client against the manifest URL base.

If capture parameters change and a new init segment is generated, a new `#EXT-X-MAP` directive is inserted at the point in the manifest where the new init segment takes effect. HLS clients handle multiple `#EXT-X-MAP` directives correctly — each segment is decoded using the init segment specified by the most recent preceding `#EXT-X-MAP` directive.

---

## 3. Server-Side State

The server maintains the following state per camera for playback:

| State | Location | Description |
|-------|----------|-------------|
| Segment metadata | Redis | `segment_id`, `start_ts`, `end_ts`, `size_bytes` per segment. Updated from `recording_segment` and `segment_evicted` alerts. TTL matches the 72-hour retention window. |
| Current manifest | In-memory | Latest M3U8 pushed by the camera. Replaced on each manifest push stream. Lost on server restart — camera re-pushes on reconnect. |
| In-flight segment map | In-memory | Per-`segment_id` upload state for request coalescing (see §5). |

The manifest is never written to Redis. Segment data is never stored on the server beyond the transient in-flight buffer.

---

## 4. Upload Mechanism

The camera's connection is outbound-only — the server cannot initiate a connection to the camera. All data transfers from camera to server use the existing QUIC connection, with the camera opening new outbound unidirectional streams as needed.

### 4.1 Segment upload

When a client requests a segment not currently in the server's in-flight map, the server issues an `upload_segment` command to the camera on the Commands stream:

```json
{
  "type": "upload_segment",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000"
}
```

The camera locates the corresponding `.m4s` file, opens a new outbound QUIC unidirectional stream, writes the raw file bytes, and closes the stream. The server correlates the upload stream to the pending command by arrival order — at most one segment upload is in flight per camera at any time, so no stream-level header is needed.

On completion the camera sends a `segment_uploaded` alert:

```json
{
  "type": "segment_uploaded",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000"
}
```

If the segment has been evicted from the ring buffer before the upload completes, the camera sends a `segment_upload_failed` alert:

```json
{
  "type": "segment_upload_failed",
  "seq": 12,
  "segment_id": "pi-zero-a1b2:1700000010000",
  "reason": "evicted"
}
```

| Reason | Description |
|--------|-------------|
| `evicted` | Segment was evicted from the ring buffer before upload could complete |
| `not_found` | Segment ID not recognised |
| `io_error` | Local filesystem read error |

### 4.2 Init segment upload

When a client begins an HLS session and requests the init segment, the server issues an `upload_init` command:

```json
{
  "type": "upload_init",
  "seq": 11
}
```

The camera opens a new outbound QUIC unidirectional stream, writes the raw `init.mp4` bytes, and closes the stream. The server correlates the upload stream by arrival order — at most one init upload is in flight per camera at any time. No alert is sent on completion — the server infers completion from stream close.

### 4.3 Manifest push

The camera pushes an updated manifest to the server whenever the manifest changes — on segment finalisation and on eviction. The camera opens a new outbound QUIC unidirectional stream, writes the raw M3U8 bytes encoded as UTF-8, and closes the stream. The server replaces its in-memory manifest on stream close. No command or alert is involved — the push is fully camera-initiated.

On reconnect the camera pushes the current manifest immediately after the handshake as part of the startup sequence (see `wire-protocol.md` §8.1).

---

## 5. Request Coalescing

The server maintains an in-flight segment map keyed on `segment_id` to avoid issuing duplicate `upload_segment` commands for the same segment:

| State | Description |
|-------|-------------|
| `Uploading` | Upload is in progress. Subsequent requests for this segment wait for the upload to complete and are served from the same buffer. |
| `Buffered` | Upload is complete. Segment data is held in memory for 60 seconds after the last byte is served. Requests within this window are served from the buffer without a new upload command. |
| `Expired` | Buffer TTL has elapsed. Entry is removed from the map. The next request triggers a fresh upload. |

The 60-second buffer window ensures that clients scrubbing back and forth over the same segment do not trigger redundant camera uploads.

---

## 6. HLS API

The server exposes three endpoints for HLS playback. All endpoints are scoped to a `device_id` and require observer authentication; ownership is verified against the authenticated user before serving.

```
GET /hls/{device_id}/init.mp4          -> fMP4 init segment (on-demand upload)
GET /hls/{device_id}/playlist.m3u8     -> current manifest (served from memory)
GET /hls/{device_id}/{segment_id}.m4s  -> fMP4 segment (on-demand upload, coalesced)
```

Segment URLs in the manifest use the `segment_id` as the filename, matching the `{segment_id}.m4s` endpoint pattern. The client resolves segment URLs relative to the manifest URL base — no absolute URLs are embedded in the manifest.

If a requested segment has been evicted from the camera's ring buffer, the server returns `404`. The client MUST re-fetch the manifest on a `404` response to get an updated view of available segments.

`Cache-Control` headers on segment responses allow the client browser to cache segments locally:

```
Cache-Control: private, max-age=3600
```

Segments are immutable once written — a given `segment_id` always refers to the same bytes — so aggressive client-side caching is safe.

---

## 7. Live Subscriber Demand Tracking

Live subscriber demand tracking is specified in `ingest.md` §8. In summary: clients in `"playback"` or `"map"` mode do not count toward live video or audio demand. When no clients are in `"live"` mode for a given camera, the server sends `stop_video` and `stop_audio` to that camera, conserving bandwidth. Segment uploads over the existing QUIC connection continue unaffected — live streaming and segment uploads share the connection via natural QUIC stream multiplexing.

---

## 8. Client Player Model

The client maintains two players per camera behind a single unified viewport:

- **Live player** — WebRTC `RTCPeerConnection`, always connected while a session is active
- **Playback player** — `<video>` element driven by hls.js (or native HLS on Safari)

Only one player is visible at a time. Switching between them is a CSS visibility swap with no layout change. Playback controls (play, pause, seek) operate directly on the hls.js instance or native HLS `<video>` element — no server communication is required for these operations. See `ui.md` for playback control behaviour across view modes.

### 8.1 Mode switching

On user-initiated scrub to a historic timestamp:
1. Client sends `client_mode: "playback"` on the reliable commands data channel
2. Client initialises hls.js with `/hls/{device_id}/playlist.m3u8`
3. hls.js fetches the manifest, resolves the target segment, fetches init and segment
4. Playback player becomes visible; live player is hidden
5. Live audio is muted (WebRTC audio track continues receiving but is not rendered)

On user return to live:
1. Client sends `client_mode: "live"` on the reliable commands data channel
2. Live player becomes visible; playback player is hidden
3. hls.js instance is destroyed
4. Live audio is unmuted

### 8.2 Init segment handling across reboots

If the camera reboots mid-session and capture parameters change, the manifest will contain multiple `#EXT-X-MAP` directives. The client follows these directives naturally as it fetches segments — each segment is decoded using the init segment specified by the most recent preceding `#EXT-X-MAP` entry in the manifest. No explicit reboot detection is required.

### 8.3 HLS player detection

```javascript
if (video.canPlayType('application/vnd.apple.mpegurl')) {
  // Native HLS — Safari
  video.src = `/hls/${deviceId}/playlist.m3u8`;
} else {
  // hls.js — Chrome, Firefox, others
  const hls = new Hls();
  hls.loadSource(`/hls/${deviceId}/playlist.m3u8`);
  hls.attachMedia(video);
}
```

### 8.4 Scrub outside available window

If the user scrubs to a timestamp outside the camera's ring buffer (segment returns `404`), the client re-fetches the manifest, updates the timeline scrubber to reflect the available window, and seeks to the nearest available segment.

### 8.5 Historic telemetry

When the client enters playback mode it fetches historic telemetry for the visible timeline window via REST (see `telemetry.md`). Live telemetry continues flowing over the WebRTC data channel and is displayed as current device state independent of the playback position.

---

## 9. Open Questions

| Question | Notes |
|----------|-------|
| Bandwidth contention between live streaming and segment upload | Segment uploads share the camera's QUIC connection with live video and audio streams. On constrained links this could degrade live stream quality. The upload handler has no throttling mechanism specified. Options include QUIC stream priority, a max upload rate cap, or deferring uploads when live subscribers are active. |
