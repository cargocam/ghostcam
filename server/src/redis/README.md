# redis

Redis integration for telemetry storage and query. Uses Redis Streams (`XADD` / `XRANGE` / `XREVRANGE`) as a time-series store — each camera has a stream at key `telemetry:{device_id}`.

Entirely optional: if `GHOSTCAM_REDIS_URL` is unset or empty, `RedisManager::new()` returns `None` and all telemetry API endpoints return `503 Service Unavailable`. Live telemetry via SSE is unaffected.

## Data Model

Each telemetry sample is an XADD entry with fields matching `TelemetryDatagram` field names. The stream ID is the Redis auto-generated millisecond timestamp + sequence number, giving sub-millisecond ordering for free.

```
XADD telemetry:cam-01 * cpu 42.1 temp 58.3 mem 312.0 ...
```

## Retention

Telemetry streams are trimmed two ways:

1. **Inline on write** — every `XADD` includes `MINID ~ <cutoff>` so Redis trims stale entries as new data arrives (see `telemetry.rs`).
2. **Background purge** — `spawn_telemetry_purge()` runs `purge_old_telemetry()` every hour, scanning all `telemetry:*` keys and issuing `XTRIM MINID ~` on each. This catches offline cameras whose streams would otherwise never be trimmed.

The retention window is controlled by `ghostcam::config::TELEMETRY_RETENTION_SECS` (default 7 days).

## Files

| File | Purpose |
|------|---------|
| `connection.rs` | `RedisManager` — connection pool, `Option` wrapper (disabled when no URL configured) |
| `telemetry.rs` | `write_telemetry` / `write_telemetry_batch` — XADD a `TelemetryDatagram` to the camera's stream with inline MINID trim |
| `telemetry_query.rs` | `query_range` — XRANGE/XREVRANGE with time bounds and limit; `query_latest` — XREVRANGE count 1 |
| `telemetry_api.rs` | Axum handlers: `handle_range` (query params → JSON array), `handle_latest` (single entry) |
| `manifest.rs` | Store and serve HLS manifests via Redis for cross-node HLS serving |
| `revocation.rs` | Redis-backed revocation list for multi-node deployments |
| `purge.rs` | `purge_device_data` — delete all Redis data for a device on unregister; `purge_old_telemetry` — XTRIM all telemetry streams older than retention window; `spawn_telemetry_purge` — background task running the purge hourly |
