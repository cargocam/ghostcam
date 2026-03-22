# server-core/src/redis
Optional Redis integration for telemetry history, segment metadata, and revocation synchronization.

Redis is optional in deployment; handlers and ingest paths degrade gracefully when unavailable, except for revocation persistence where warnings are emitted.

## Modules
- `connection.rs`: reconnecting `RedisManager` wrapper.
- `telemetry.rs`: write telemetry datagrams to Redis streams.
- `telemetry_query.rs`: range/latest queries with cursor pagination.
- `telemetry_api.rs`: Axum handlers for telemetry endpoints.
- `segments.rs`: segment metadata upsert/list/delete helpers.
- `revocation.rs`: revoked cert set persistence + cache refresh loop.
- `purge.rs`: device-scoped cleanup utilities.

## Key schema
### Telemetry stream
- key: `telemetry:<device_id>`
- command: `XADD` with retention by approximate `MINID`
- retention window: 72 hours (`RETENTION_MS`)

Each entry stores `ts`, `server_ts`, and optional telemetry fields.

### Segment metadata
- key: `segments:<device_id>:<segment_id>`
- value: hash with `segment_id`, `start_ts`, `end_ts`, `size_bytes`
- TTL: 72 hours

### Revocation set
- key: `revoked_certs`
- value: set members of revoked serial/fingerprint identifiers

## Connection behavior
`RedisManager`:
- attempts non-fatal initial connect,
- tracks `connected` flag and write-error counter,
- offers background reconnect loop with exponential backoff.

## API behavior
Telemetry API handlers return:
- `503` when Redis is not configured/connected,
- `404` for latest when no records exist,
- paginated range pages with optional `next_cursor`.

## Purge behavior
On unregister flows, `purge_device_data()` removes:
- `telemetry:<device_id>` stream,
- all `segments:<device_id>:*` keys.
