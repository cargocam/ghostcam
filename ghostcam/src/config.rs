/// QUIC listener port.
pub const QUIC_PORT: u16 = 4433;

/// HTTP API + static file serving port.
pub const HTTP_PORT: u16 = 3000;

/// Broadcast channel capacity for frame distribution.
/// At 30fps video, 2048 frames ≈ 68 seconds of buffer before lagging.
pub const BROADCAST_CAPACITY: usize = 2048;

/// Maximum length-prefixed frame size (4 MB).
pub const MAX_FRAME_SIZE: u32 = 4 * 1024 * 1024;

/// QUIC keepalive interval.
pub const KEEPALIVE_INTERVAL_SECS: u64 = 15;

/// Disconnect timeout (no keepalive received).
pub const DISCONNECT_TIMEOUT_SECS: u64 = 30;

/// Telemetry polling interval on camera.
pub const TELEMETRY_POLL_INTERVAL_SECS: u64 = 2;

/// Full telemetry heartbeat interval.
pub const TELEMETRY_HEARTBEAT_INTERVAL_SECS: u64 = 30;

/// Maximum telemetry datagrams buffered on camera for upload.
pub const TELEMETRY_BUFFER_CAP: usize = 100_000;

/// fMP4 segment duration.
pub const SEGMENT_DURATION_SECS: u64 = 10;

/// Segment coalescing buffer TTL.
pub const SEGMENT_BUFFER_TTL_SECS: u64 = 60;

/// Enrollment token lifetime.
pub const ENROLLMENT_TOKEN_TTL_SECS: u64 = 600;

/// Session cookie lifetime.
pub const SESSION_TTL_DAYS: u64 = 30;

/// Redis telemetry stream retention.
pub const TELEMETRY_RETENTION_HOURS: u64 = 72;

/// Revocation cache refresh interval.
pub const REVOCATION_CACHE_REFRESH_SECS: u64 = 60;

/// Initial reconnect backoff.
pub const RECONNECT_BACKOFF_INITIAL_SECS: u64 = 1;

/// Maximum reconnect backoff.
pub const RECONNECT_BACKOFF_MAX_SECS: u64 = 30;

/// Maximum concurrent QUIC connection handshakes.
pub const MAX_CONCURRENT_CONNECTIONS: usize = 256;

/// Default PostgreSQL connection pool size.
pub const DEFAULT_DB_POOL_SIZE: u32 = 20;

/// Maximum concurrent WebRTC sessions per user.
pub const MAX_SESSIONS_PER_USER: usize = 50;

/// Maximum concurrent WebRTC viewers per camera.
pub const MAX_VIEWERS_PER_CAMERA: usize = 20;

/// Segment cache cleanup interval.
pub const SEGMENT_CLEANUP_INTERVAL_SECS: u64 = 30;

/// Wire protocol version.
pub const PROTOCOL_VERSION: u32 = 1;
