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

/// Redis telemetry stream retention (seconds). Default 7 days.
pub const TELEMETRY_RETENTION_SECS: u64 = 7 * 24 * 60 * 60;

/// Revocation cache refresh interval.
pub const REVOCATION_CACHE_REFRESH_SECS: u64 = 60;

/// Initial reconnect backoff.
pub const RECONNECT_BACKOFF_INITIAL_SECS: u64 = 1;

/// Maximum reconnect backoff.
pub const RECONNECT_BACKOFF_MAX_SECS: u64 = 30;

/// Maximum concurrent bidirectional QUIC streams per connection.
/// Cameras use 1 (alerts). Small headroom for protocol evolution.
pub const QUIC_MAX_BIDI_STREAMS: u32 = 4;

/// Maximum concurrent unidirectional QUIC streams per connection.
/// Cameras open Video + Audio (persistent) + upload streams (transient).
pub const QUIC_MAX_UNI_STREAMS: u32 = 16;

/// Maximum concurrent QUIC connections the server will accept.
pub const QUIC_MAX_CONNECTIONS: u32 = 256;

/// Maximum HTTP request body size in bytes (1 MB).
pub const MAX_REQUEST_BODY_BYTES: usize = 1_048_576;

/// Telemetry batch flush interval (seconds).
pub const TELEMETRY_BATCH_INTERVAL_SECS: u64 = 5;

/// Maximum WebRTC sessions per user.
pub const MAX_SESSIONS_PER_USER: usize = 20;

/// Wire protocol version.
pub const PROTOCOL_VERSION: u32 = 1;
