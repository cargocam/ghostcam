# Plan 5: Redis Integration

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 3 (ingest pipeline), Plan 4 (revocation cache interface)
**Unlocks:** Plan 6 (egress & HTTP API — telemetry REST already done here)

---

## 1. Goal

Implement all Redis-backed persistence: telemetry write path (live datagrams + buffered uploads), telemetry REST API, segment metadata storage, and the revocation cache refresh loop. Implement graceful degradation when Redis is unavailable.

After this plan, telemetry datagrams are persisted to Redis Streams, historic telemetry is queryable via REST, segment metadata is indexed, the revocation cache refreshes from Redis every 60 seconds, and all Redis operations degrade gracefully on failure.

---

## 2. Crate Changes

### 2.1 New Dependencies

**Workspace `Cargo.toml`** — add:
```toml
[workspace.dependencies]
redis = { version = "0.27", features = ["tokio-comp", "streams"] }
axum = "0.7"
axum-extra = { version = "0.9", features = ["cookie"] }
tower = "0.5"
tower-http = { version = "0.6", features = ["cors", "trace"] }
```

**`server-core/Cargo.toml`** — add:
```toml
[dependencies]
redis.workspace = true
axum.workspace = true
```

---

## 3. Implementation Details

### 3.1 Redis Connection Manager (`server-core/src/redis/connection.rs`)

Wraps the Redis connection with automatic reconnection and health tracking.

```rust
pub struct RedisManager {
    /// Active connection. None if Redis is unavailable.
    conn: RwLock<Option<MultiplexedConnection>>,
    /// Redis URL for reconnection.
    url: String,
    /// Error counter for monitoring.
    write_errors: AtomicU64,
    /// Whether Redis has ever been connected.
    connected: AtomicBool,
}

impl RedisManager {
    /// Create a new manager. Attempts initial connection but does not fail
    /// if Redis is unavailable — logs a warning and starts with conn=None.
    pub async fn new(redis_url: &str) -> Self;

    /// Get a connection. Returns None if Redis is unavailable.
    pub async fn get_conn(&self) -> Option<MultiplexedConnection>;

    /// Check if Redis is currently connected.
    pub fn is_connected(&self) -> bool;

    /// Get the write error count (for metrics/health).
    pub fn write_error_count(&self) -> u64;

    /// Spawn a background reconnect loop that retries with exponential backoff
    /// (1s initial, 30s cap). On recovery, sets conn and logs.
    pub fn spawn_reconnect_loop(self: &Arc<Self>, cancel: CancellationToken);
}
```

### 3.2 Telemetry Writer (`server-core/src/redis/telemetry.rs`)

Writes telemetry datagrams to Redis Streams.

```rust
const TELEMETRY_KEY_PREFIX: &str = "telemetry:";
const RETENTION_MS: u64 = 72 * 60 * 60 * 1000; // 72 hours

/// Write a single live telemetry datagram to Redis.
///
/// Uses XADD with MINID approximate trimming for 72h retention.
/// Redis stream ID is server_ts (receipt time in ms).
///
/// Drops silently and increments error counter if Redis is unavailable.
pub async fn write_telemetry(
    redis: &RedisManager,
    device_id: &DeviceId,
    datagram: &TelemetryDatagram,
) -> Result<()> {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, "redis unavailable — dropping telemetry write");
        return Ok(());
    };

    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);
    let server_ts = now_ms();
    let min_id = server_ts.saturating_sub(RETENTION_MS);

    // XADD telemetry:{device_id} MINID ~ {min_id} {server_ts} field value ...
    // Fields are flattened from the datagram struct.
    redis::cmd("XADD")
        .arg(&key)
        .arg("MINID").arg("~").arg(min_id)
        .arg(server_ts)
        // ... field/value pairs from datagram
        .query_async(&mut conn)
        .await?;

    Ok(())
}

/// Write a batch of buffered telemetry datagrams to Redis.
/// Each entry gets its own XADD with the current server_ts.
pub async fn write_telemetry_batch(
    redis: &RedisManager,
    device_id: &DeviceId,
    datagrams: &[TelemetryDatagram],
) -> Result<()>;

/// Field mapping for XADD. Only non-None fields are written.
fn datagram_to_fields(datagram: &TelemetryDatagram, server_ts: u64) -> Vec<(String, String)>;

/// Parse Redis stream entry fields back into a TelemetryEntry.
fn fields_to_entry(fields: &[(String, String)]) -> Result<TelemetryEntry>;
```

Response type for the REST API:

```rust
#[derive(Debug, Serialize)]
pub struct TelemetryEntry {
    pub ts: u64,
    pub server_ts: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sig: Option<i8>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temp: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fps: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kbps: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uptime: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lat: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lon: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alt: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gps_fix: Option<u8>,
}
```

### 3.3 Telemetry Reader (`server-core/src/redis/telemetry_query.rs`)

Reads telemetry from Redis for the REST API.

```rust
/// Fetch the latest telemetry entry for a camera.
/// Redis: XREVRANGE telemetry:{device_id} + - COUNT 1
pub async fn get_latest(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<Option<TelemetryEntry>>;

/// Fetch telemetry entries within a time range with pagination.
///
/// Filters on the `ts` field (camera clock), not the Redis stream ID.
/// Paginates internally until `limit` matching entries are found or stream exhausted.
///
/// Returns entries + optional next_cursor (Redis stream ID of last entry).
pub async fn query_range(
    redis: &RedisManager,
    device_id: &DeviceId,
    from_ts: u64,
    to_ts: u64,
    cursor: Option<&str>,
    limit: usize,
) -> Result<TelemetryPage>;

#[derive(Debug, Serialize)]
pub struct TelemetryPage {
    pub entries: Vec<TelemetryEntry>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub next_cursor: Option<String>,
}

const MAX_LIMIT: usize = 3600;
const DEFAULT_LIMIT: usize = 3600;
```

The `query_range` implementation:
1. Start from `cursor` (or stream beginning `"-"`)
2. `XRANGE telemetry:{device_id} {start} + COUNT {batch_size}` where batch_size is 2×limit (to account for ts filtering)
3. For each entry, check if `ts` falls within `[from_ts, to_ts]`
4. Collect matching entries until `limit` reached or stream exhausted
5. If more entries may exist, set `next_cursor` to the stream ID of the last returned entry

### 3.4 Telemetry REST Handlers (`server-core/src/redis/telemetry_api.rs`)

Standalone Axum handlers. Auth middleware is not wired in this plan — Plan 6 adds auth. For now, these handlers accept any request (ownership verification deferred to Plan 6).

```rust
/// GET /telemetry/{device_id}/latest
pub async fn handle_latest(
    Path(device_id): Path<String>,
    State(redis): State<Arc<RedisManager>>,
) -> Result<Json<TelemetryEntry>, StatusCode> {
    if !redis.is_connected() {
        return Err(StatusCode::SERVICE_UNAVAILABLE);
    }
    let device_id = DeviceId(device_id);
    match get_latest(&redis, &device_id).await {
        Ok(Some(entry)) => Ok(Json(entry)),
        Ok(None) => Err(StatusCode::NOT_FOUND),
        Err(_) => Err(StatusCode::INTERNAL_SERVER_ERROR),
    }
}

/// GET /telemetry/{device_id}?from={}&to={}&cursor={}&limit={}
pub async fn handle_range(
    Path(device_id): Path<String>,
    Query(params): Query<TelemetryRangeParams>,
    State(redis): State<Arc<RedisManager>>,
) -> Result<Json<TelemetryPage>, StatusCode>;

#[derive(Deserialize)]
pub struct TelemetryRangeParams {
    pub from: u64,
    pub to: u64,
    pub cursor: Option<String>,
    pub limit: Option<usize>,
}
```

### 3.5 Segment Metadata (`server-core/src/redis/segments.rs`)

Stores segment metadata from `recording_segment` alerts. Used for playback window queries.

```rust
const SEGMENT_KEY_PREFIX: &str = "segments:";
const SEGMENT_TTL_SECS: u64 = 72 * 60 * 60; // 72 hours

/// Upsert segment metadata to Redis.
/// Key: segments:{device_id}:{segment_id} → Hash { start_ts, end_ts, size_bytes }
/// TTL: 72 hours
pub async fn upsert_segment(
    redis: &RedisManager,
    device_id: &DeviceId,
    segment_id: &str,
    start_ts: u64,
    end_ts: u64,
    size_bytes: u64,
) -> Result<()>;

/// Tombstone a segment (delete the key). Called on `segment_evicted`.
pub async fn delete_segment(
    redis: &RedisManager,
    device_id: &DeviceId,
    segment_id: &str,
) -> Result<()>;

/// List all segment metadata for a camera, sorted by start_ts.
/// Used for playback window queries.
pub async fn list_segments(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<Vec<SegmentMetadata>>;

/// Delete all segment metadata for a camera. Called on unregistration.
pub async fn purge_segments(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<()>;

#[derive(Debug, Serialize, Deserialize)]
pub struct SegmentMetadata {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
}
```

Implementation uses Redis HASH per segment with EXPIRE for TTL. `list_segments` uses SCAN to find matching keys (pattern: `segments:{device_id}:*`), then HGETALL on each.

### 3.6 Revocation Cache Refresh (`server-core/src/redis/revocation.rs`)

Wires the RevocationCache from Plan 4 to Redis.

```rust
const REVOCATION_KEY: &str = "revoked_certs";

/// Spawn a background task that refreshes the revocation cache from Redis
/// every REVOCATION_CACHE_REFRESH_SECS (60s).
///
/// On startup: if Redis is available, load immediately. If not, cache starts empty.
/// On each tick: SMEMBERS revoked_certs → replace cache contents.
/// On Redis error: log warning, retain stale cache, retry next tick.
pub fn spawn_revocation_refresh(
    redis: Arc<RedisManager>,
    cache: Arc<RevocationCache>,
    cancel: CancellationToken,
) -> JoinHandle<()>;

/// Add a serial number to the Redis revocation set.
/// Called during unregistration.
pub async fn revoke_cert(
    redis: &RedisManager,
    serial: &str,
) -> Result<()>;

/// Remove all revocation entries for a device's certificates.
/// Called during data purge on unregistration.
pub async fn purge_revocations(
    redis: &RedisManager,
    serials: &[String],
) -> Result<()>;
```

### 3.7 Telemetry Purge (`server-core/src/redis/purge.rs`)

Called on camera unregistration to remove all Redis data for a device.

```rust
/// Purge all Redis data for a device: telemetry stream + segment metadata.
pub async fn purge_device_data(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<()> {
    // DEL telemetry:{device_id}
    // SCAN + DEL segments:{device_id}:*
}
```

### 3.8 Ingest Pipeline Wiring

Update the alert handler from Plan 3 to write to Redis:

```rust
// In alerts.rs — update handlers that were previously stubs:

Alert::RecordingSegment { segment_id, start_ts, end_ts, size_bytes, .. } => {
    segments::upsert_segment(redis, &slot.device_id, &segment_id, start_ts, end_ts, size_bytes).await?;
}

Alert::SegmentEvicted { segment_id } => {
    segments::delete_segment(redis, &slot.device_id, &segment_id).await?;
}
```

Update the telemetry reader in the IngestSlot to write to Redis concurrently with broadcast:

```rust
// In slot.rs telemetry_reader task:
// On each datagram:
//   1. broadcast to telemetry_tx (non-blocking)
//   2. spawn write_telemetry to Redis (concurrent, non-blocking)
```

Update the telemetry buffer upload handler:

```rust
// In uploads.rs handle_telemetry_buffer:
// Decode MessagePack array, call write_telemetry_batch
```

Update unregistration to purge Redis data:

```rust
// In unregister.rs, after ack received:
// 1. purge_device_data (telemetry + segments)
// 2. revoke_cert (add serial to revoked_certs set)
```

### 3.9 Module Structure

```
server-core/src/
├── redis/
│   ├── mod.rs                   # re-exports
│   ├── connection.rs            # RedisManager
│   ├── telemetry.rs             # write_telemetry, write_telemetry_batch
│   ├── telemetry_query.rs       # get_latest, query_range
│   ├── telemetry_api.rs         # Axum handlers for REST endpoints
│   ├── segments.rs              # Segment metadata CRUD
│   ├── revocation.rs            # Revocation refresh loop + revoke_cert
│   └── purge.rs                 # Device data purge
```

---

## 4. Testing Plan

### 4.1 Test Infrastructure

Tests that require Redis use a real Redis instance. They are gated behind `#[cfg(feature = "redis-tests")]` or `#[ignore]`. Each test uses a unique key prefix (e.g., `test:{uuid}:`) to avoid collisions, and cleans up after itself.

```rust
/// Test helper: connect to Redis at localhost:6379 (default),
/// return a RedisManager + a unique key prefix for isolation.
struct RedisTestEnv {
    redis: Arc<RedisManager>,
    prefix: String,
}

impl RedisTestEnv {
    async fn setup() -> Self;
    async fn teardown(&self); // DEL all keys matching prefix
}
```

For unit tests that don't need Redis, use the `RedisManager` with `conn=None` to verify graceful degradation.

### 4.2 Unit Tests — RedisManager (no Redis needed)

**Location:** `server-core/src/redis/connection.rs`

| Test | Description |
|------|-------------|
| `new_without_redis` | Create manager with bad URL → conn is None, is_connected() false, no panic |
| `get_conn_when_disconnected` | Manager with conn=None → get_conn() returns None |
| `write_error_counter_starts_zero` | New manager → write_error_count() == 0 |

### 4.3 Unit Tests — Telemetry Field Mapping

**Location:** `server-core/src/redis/telemetry.rs`

| Test | Description |
|------|-------------|
| `datagram_to_fields_full` | Full datagram → all fields present in output |
| `datagram_to_fields_sparse` | Datagram with only ts + cpu → only ts, server_ts, cpu in output |
| `datagram_to_fields_gps` | Datagram with GPS → lat, lon, alt, gps_fix present |
| `datagram_to_fields_no_gps` | Datagram without GPS → GPS fields absent |
| `fields_to_entry_roundtrip` | datagram_to_fields → fields_to_entry → matches original values |
| `fields_to_entry_missing_optional` | Fields with only ts + server_ts → entry has all optionals as None |

### 4.4 Unit Tests — Telemetry Query Parameters

**Location:** `server-core/src/redis/telemetry_query.rs`

| Test | Description |
|------|-------------|
| `limit_clamped_to_max` | limit=10000 → clamped to 3600 |
| `limit_default` | limit=None → uses 3600 |

### 4.5 Integration Tests — Telemetry Write + Read

**Location:** `server-core/tests/redis_telemetry.rs` (requires Redis)

| Test | Description |
|------|-------------|
| `write_and_read_latest` | Write one datagram → get_latest returns it with correct fields |
| `write_multiple_read_latest` | Write 3 datagrams → get_latest returns the last one |
| `write_sparse_datagram` | Write datagram with only ts + cpu → get_latest returns entry with cpu, other fields None |
| `write_with_gps` | Write datagram with GPS fields → get_latest returns them correctly |
| `query_range_all` | Write 5 datagrams at ts=1000,2000,3000,4000,5000 → query from=1000 to=5000 → 5 entries |
| `query_range_subset` | Write 5 datagrams → query from=2000 to=4000 → 3 entries |
| `query_range_empty` | Write datagrams at ts=1000-5000 → query from=9000 to=10000 → empty entries |
| `query_range_pagination` | Write 10 datagrams → query limit=3 → 3 entries + next_cursor → query with cursor → next 3 entries |
| `query_range_pagination_exhausted` | Write 5 datagrams → query limit=10 → 5 entries, next_cursor absent |
| `write_telemetry_batch` | Write batch of 5 datagrams → query returns all 5 in order |
| `retention_trimming` | Write datagram with server_ts far in the past → write new datagram with MINID trimming → old entry eventually trimmed (verify via XLEN or XRANGE) |
| `latest_not_found` | get_latest for non-existent device → None |

### 4.6 Integration Tests — Telemetry REST API

**Location:** `server-core/tests/redis_telemetry_api.rs` (requires Redis)

Uses `axum::test` helpers or `tower::ServiceExt` to call handlers directly without a real HTTP server.

| Test | Description |
|------|-------------|
| `latest_endpoint_returns_entry` | Write datagram, GET /telemetry/{id}/latest → 200 with JSON entry |
| `latest_endpoint_not_found` | GET /telemetry/unknown/latest → 404 |
| `range_endpoint_returns_entries` | Write datagrams, GET /telemetry/{id}?from=&to= → 200 with entries array |
| `range_endpoint_pagination` | GET with limit=2 → 2 entries + next_cursor in response |
| `range_endpoint_with_cursor` | GET with cursor from previous response → next page |
| `range_endpoint_empty` | GET with out-of-range from/to → 200 with empty entries |
| `redis_unavailable_returns_503` | Manager with conn=None → GET /telemetry/{id}/latest → 503 |
| `redis_unavailable_returns_503_range` | Manager with conn=None → GET /telemetry/{id}?from=&to= → 503 |

### 4.7 Integration Tests — Segment Metadata

**Location:** `server-core/tests/redis_segments.rs` (requires Redis)

| Test | Description |
|------|-------------|
| `upsert_and_list` | Upsert 3 segments → list returns 3 sorted by start_ts |
| `upsert_idempotent` | Upsert same segment_id twice → list returns 1 (latest values) |
| `delete_segment` | Upsert then delete → list returns empty |
| `list_empty` | List for device with no segments → empty vec |
| `purge_segments` | Upsert 3 segments, purge → list returns empty |
| `segment_ttl` | Upsert segment → verify key has TTL set (via Redis TTL command) |

### 4.8 Integration Tests — Revocation Refresh

**Location:** `server-core/tests/redis_revocation.rs` (requires Redis)

| Test | Description |
|------|-------------|
| `revoke_cert_adds_to_set` | Call revoke_cert("serial-1") → SISMEMBER revoked_certs "serial-1" → true |
| `refresh_populates_cache` | Add serials to Redis set, spawn refresh task, wait one cycle → cache.is_revoked("serial-1") → true |
| `refresh_replaces_cache` | Initial set has "a","b". After refresh, remove "a" from Redis, add "c". Wait for next cycle → "a" not revoked, "b" revoked, "c" revoked |
| `refresh_survives_redis_outage` | Populate cache, simulate Redis disconnect → cache retains stale data |
| `purge_revocations` | Add serials, purge → SISMEMBER returns false |

### 4.9 Integration Tests — Graceful Degradation

**Location:** `server-core/tests/redis_degradation.rs`

| Test | Description |
|------|-------------|
| `write_telemetry_without_redis` | Manager with conn=None → write_telemetry returns Ok, no panic |
| `write_batch_without_redis` | Manager with conn=None → write_telemetry_batch returns Ok |
| `upsert_segment_without_redis` | Manager with conn=None → upsert_segment returns Ok (or logs warning) |
| `delete_segment_without_redis` | Manager with conn=None → returns Ok |
| `revoke_cert_without_redis` | Manager with conn=None → returns error (revocation is important enough to surface) |

### 4.10 Integration Tests — Ingest Wiring

**Location:** `server-core/tests/redis_ingest_wiring.rs` (requires Redis)

Uses `TestEnv` from Plan 3 extended with Redis.

| Test | Description |
|------|-------------|
| `camera_telemetry_persisted` | MockCamera sends telemetry datagram → get_latest returns it from Redis |
| `camera_telemetry_broadcast_and_persisted` | MockCamera sends datagram → both broadcast subscriber AND Redis have the data |
| `recording_segment_alert_persisted` | MockCamera sends recording_segment alert → list_segments returns it |
| `segment_evicted_alert_removes` | MockCamera sends recording_segment then segment_evicted → list_segments empty |
| `telemetry_buffer_upload_persisted` | MockCamera sends telemetry buffer upload stream → all entries appear in Redis |

### 4.11 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All crates compile |
| Unit tests (no Redis) | `cargo test -p server-core` (non-ignored) | Pass |
| Redis integration tests | `cargo test -p server-core -- --ignored` | Pass (requires Redis at localhost:6379) |
| Clippy | `cargo clippy -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 5. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test` passes all non-Redis tests
- [ ] Redis integration tests pass with a running Redis instance
- [ ] Telemetry datagrams are written to Redis Streams with correct field mapping
- [ ] Sparse datagrams only write non-None fields to Redis
- [ ] GPS fields are correctly persisted and retrieved
- [ ] `MINID` approximate trimming is applied on every XADD
- [ ] get_latest returns the most recent entry by stream ID
- [ ] query_range filters by `ts` (camera clock), not stream ID
- [ ] Pagination works: cursor from response can be used to fetch next page
- [ ] Pagination terminates: last page has no next_cursor
- [ ] Limit is clamped to 3600
- [ ] Telemetry batch writes persist all entries
- [ ] Segment metadata is stored as Redis hashes with 72h TTL
- [ ] Segment upsert is idempotent
- [ ] Segment delete removes the key
- [ ] purge_device_data removes telemetry stream + all segment keys
- [ ] Revocation refresh loop populates cache from Redis SMEMBERS
- [ ] Revocation cache is refreshed every 60 seconds
- [ ] revoke_cert adds serial to Redis set
- [ ] Graceful degradation: all write operations return Ok when Redis is unavailable
- [ ] Telemetry REST endpoints return 503 when Redis is unavailable
- [ ] Telemetry REST latest returns 404 for unknown device
- [ ] Alert handler writes to Redis on recording_segment and segment_evicted
- [ ] Telemetry reader in IngestSlot writes to Redis concurrently with broadcast
- [ ] Telemetry buffer upload handler writes batch to Redis
- [ ] `CLAUDE.md` updated with Redis module structure
