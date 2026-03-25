# Ghostcam — Telemetry

**Status:** Draft

---

## 1. Overview

This document specifies the telemetry persistence model, Redis data structures, write path, and REST query API for historic telemetry access.

Live telemetry delivery over the WebRTC data channel is specified in `wire-protocol.md` §5 and `webrtc-client.md` §4. The camera-side telemetry send loop and buffer are specified in `camera-firmware.md` §7.

---

## 2. Persistence Model

The server persists all incoming telemetry to a Redis cluster using one Redis Stream per camera:

```
telemetry:{device_id}
```

Telemetry arrives via two paths: live datagrams received continuously over QUIC, and buffered entries uploaded on reconnect via a dedicated QUIC stream. Both paths are persisted to the same Redis Stream using the same schema.

Server-injected fields are never persisted — they reflect transient observer state and are not part of the historic record.

### 2.1 Live datagram write path

On receipt of a live telemetry datagram from the camera the server:

1. Decodes the MessagePack payload
2. Records a server-side receipt timestamp (`server_ts`)
3. Writes the entry to the Redis Stream via `XADD telemetry:{device_id} {server_ts} field value ...`
4. Concurrently broadcasts the datagram to subscribed egress handles

Steps 3 and 4 are concurrent — Redis writes do not block the fan-out path.

### 2.2 Buffered telemetry write path

On receipt of a telemetry buffer upload stream from the camera the server:

1. Reads the full stream and decodes the MessagePack array
2. For each entry, records a server-side receipt timestamp (`server_ts`)
3. Writes each entry to the Redis Stream via `XADD telemetry:{device_id} {server_ts} field value ...`

Buffered entries are written in the order they appear in the upload stream. Because `server_ts` is used as the stream ID, buffered entries written on reconnect will have later stream IDs than live entries written before the disconnection — this is correct and expected. Time-range queries use the `ts` field (camera clock) rather than the stream ID for filtering, ensuring buffered entries are correctly placed in the historic record relative to their recording time.

### 2.3 Redis stream ID

The Redis stream ID uses `server_ts` (server receipt time in milliseconds) rather than the camera clock `ts`. This guarantees monotonically increasing stream IDs regardless of camera clock drift or out-of-order delivery. The camera clock timestamp `ts` is preserved as a field in every entry and is used by clients for display and time-range filtering.

### 2.4 Retention

Telemetry entries are trimmed using `MINID` to a rolling 72-hour window. This is an independent policy — the camera's footage retention is storage-capped with no time limit, and the two retention windows are not required to be in sync. 72 hours of telemetry history is sufficient for all practical scrubbing use cases.

```
XADD telemetry:{device_id} MINID ~ {now_ms - 72h_ms} {server_ts} field value ...
```

The `~` approximate trimming operator allows Redis to trim in bulk at natural boundaries rather than on every write, reducing overhead. Occasional entries slightly outside the 72-hour window may be retained transiently — this is acceptable.

### 2.5 Persisted fields

| Field | Type | Description |
|-------|------|-------------|
| `ts` | `u64` | Camera clock timestamp, Unix ms |
| `server_ts` | `u64` | Server receipt timestamp, Unix ms |
| `sig` | `i8` | WiFi signal strength (dBm) |
| `temp` | `u32` | SoC temperature (°C) |
| `fps` | `f32` | Video capture frame rate |
| `kbps` | `u32` | Video bitrate (kbps) |
| `cpu` | `u32` | CPU usage (%) |
| `mem` | `u32` | Memory usage (MB) |
| `uptime` | `u32` | System uptime (seconds) |
| `lat` | `f64` | GPS latitude (decimal degrees) |
| `lon` | `f64` | GPS longitude (decimal degrees) |
| `alt` | `f32` | GPS altitude (metres) |
| `gps_fix` | `u8` | GPS fix quality |

All fields except `ts` and `server_ts` are optional — a datagram that omits a field results in that field being absent from the Redis entry. Clients MUST handle absent fields gracefully.

### 2.6 Telemetry gaps

The camera telemetry buffer preserves readings across disconnections by writing to disk rather than dropping datagrams. Deduplication ensures identical heartbeat runs do not inflate the buffer. The buffer is capped at 100,000 entries — when full, the oldest entries are evicted to make room for new ones. In practice the dedup logic means a run of identical heartbeats compresses to two entries, so the effective coverage before eviction is far longer than the raw entry count suggests. Entries are uploaded on reconnect and cleared from disk on successful transfer. Clients should treat absent telemetry for a time range as a gap rather than an error.

---

## 3. REST API

The server exposes two telemetry endpoints. Both require observer authentication; ownership of the `device_id` is verified against the authenticated user before any Redis query is issued. Time-range filtering is performed on the `ts` field (camera clock) rather than the Redis stream ID.

### 3.1 Latest value

```
GET /telemetry/{device_id}/latest
```

Returns the most recent telemetry entry for the camera, ordered by `ts`.

**Redis query:**
```
XREVRANGE telemetry:{device_id} + - COUNT 1
```

**Response:**
```json
{
  "ts": 1700000000123,
  "server_ts": 1700000000145,
  "sig": -62,
  "temp": 54,
  "fps": 29.97,
  "kbps": 2400,
  "cpu": 23,
  "mem": 312,
  "uptime": 86400,
  "lat": 37.7749,
  "lon": -122.4194,
  "alt": 15.2,
  "gps_fix": 2
}
```

Returns `404` if no telemetry has been received for the camera.

### 3.2 Time-range query

```
GET /telemetry/{device_id}?from={unix_ms}&to={unix_ms}&cursor={stream_id}&limit={n}
```

Returns telemetry entries within the specified time range, ordered oldest to newest. Both `from` and `to` are required and filter on the `ts` field (camera clock). Results are paginated.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `from` | yes | Start of range, Unix ms (camera clock, inclusive) |
| `to` | yes | End of range, Unix ms (camera clock, inclusive) |
| `cursor` | no | Redis stream ID of the last entry from the previous page. Omit for the first page. |
| `limit` | no | Maximum entries to return. Default and maximum: 3,600. |

**Redis query:**

The server fetches a page of entries from the stream using `XRANGE`, then filters on the `ts` field to apply the `from`/`to` range:

```
XRANGE telemetry:{device_id} {cursor_or_start} + COUNT {limit}
```

Entries with `ts` outside `[from, to]` are excluded from the response. The server continues paginating internally until it has `limit` matching entries or exhausts the stream.

**Response:**
```json
{
  "entries": [
    {
      "ts": 1700000000123,
      "server_ts": 1700000000145,
      "sig": -62,
      "temp": 54,
      "fps": 29.97,
      "kbps": 2400,
      "cpu": 23,
      "mem": 312,
      "uptime": 86400,
      "lat": 37.7749,
      "lon": -122.4194,
      "alt": 15.2,
      "gps_fix": 2
    }
  ],
  "next_cursor": "1700000003600000-0"
}
```

`next_cursor` is the Redis stream ID of the last returned entry. Pass it as `cursor` on the next request to retrieve the following page. If `next_cursor` is absent, the current page is the last.

Returns an empty `entries` array if no telemetry exists for the requested range. Does not return an error if the range partially or fully falls outside the 72-hour retention window — the server returns whatever Redis holds.

### 3.3 Telemetry and footage availability

Telemetry availability and footage availability are independent. A time range query may return telemetry for a window where the corresponding footage segments have been evicted from the camera's ring buffer. The server serves whatever telemetry Redis holds regardless of footage availability. Clients are responsible for handling windows where telemetry exists but footage does not, and vice versa.

---

## 4. Client Usage

### 4.1 Initial load

On session start the client fetches the latest telemetry for each camera to populate the UI before live datagrams arrive:

```
GET /telemetry/{device_id}/latest
```

### 4.2 Playback scrubbing

When the client scrubs to a historic timestamp it fetches the telemetry window surrounding the target position using the time-range query:

```
GET /telemetry/{device_id}?from={seek_ts - window}&to={seek_ts + window}
```

The window size is a client-side decision based on the visible timeline range. Clients should fetch generously and paginate as needed — Redis time-range queries are cheap and prefetching avoids round-trips during scrubbing.

### 4.3 Live telemetry during playback

Live telemetry continues flowing over the WebRTC telemetry data channel during playback and is displayed as current device state — signal strength, temperature, GPS position — independently of the historic playback position. Clients do not attempt to correlate live telemetry with the playback timestamp.

---

## 5. Failure Modes

### 5.1 Redis unavailable — write path

When Redis is unavailable, telemetry write operations (`XADD`) fail. The server drops the write silently, increments an internal error counter, and logs a warning. The telemetry fan-out to WebRTC egress handles continues unaffected — live telemetry still reaches observers, it is just not persisted.

The server retries Redis connectivity in the background (see `ingest.md` §9.3). On recovery, telemetry writes resume for new datagrams. Datagrams dropped during the outage are not recoverable from the server side. If the camera was connected during the outage, those datagrams were already sent as live datagrams to any connected observers and are simply absent from the historic record.

### 5.2 Redis unavailable — read path

If Redis is unavailable when a telemetry REST query arrives, the server returns `503 Service Unavailable` with a `Retry-After: 30` header. No partial results are returned.
