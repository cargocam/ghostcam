# redis

Redis integration for telemetry storage and query. Uses Redis Streams (`XADD` / `XRANGE` / `XREVRANGE`) as a time-series store — each camera has a stream at key `telemetry:{device_id}`.

Entirely optional: if `GHOSTCAM_REDIS_URL` is unset or empty, `RedisManager::new()` returns `None` and all telemetry API endpoints return `503 Service Unavailable`. Live telemetry via SSE is unaffected.

## Data Model

Each telemetry sample is an XADD entry with fields matching `TelemetryDatagram` field names. The stream ID is the Redis auto-generated millisecond timestamp + sequence number, giving sub-millisecond ordering for free.

```
XADD telemetry:cam-01 * cpu 42.1 temp 58.3 mem 312.0 ...
```

## Files

| File | Purpose |
|------|---------|
| `connection.rs` | `RedisManager` — connection pool, `Option` wrapper (disabled when no URL configured) |
| `telemetry.rs` | `write_telemetry` — XADD a `TelemetryDatagram` to the camera's stream |
| `telemetry_query.rs` | `query_range` — XRANGE/XREVRANGE with time bounds and limit; `query_latest` — XREVRANGE count 1 |
| `telemetry_api.rs` | Axum handlers: `handle_range` (query params → JSON array), `handle_latest` (single entry) |
| `segments.rs` | Upload segment data to Redis for cross-process HLS serving (optional) |
| `revocation.rs` | Redis-backed revocation list for multi-node deployments |
| `purge.rs` | `purge_old_telemetry` — XTRIM streams older than retention window (called on a background interval) |
