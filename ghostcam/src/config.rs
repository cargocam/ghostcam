use std::path::Path;

use anyhow::{Context, Result};

/// QUIC listener port.
pub const QUIC_PORT: u16 = 4433;

/// HTTP API + static file serving port.
pub const HTTP_PORT: u16 = 3000;

/// Broadcast channel capacity for frame distribution.
/// At 30fps video, 128 frames ≈ 4 seconds of buffer before lagging.
/// Lagging viewers drop frames and catch up — this is intentional for
/// live surveillance (prefer low latency over lossless delivery).
pub const BROADCAST_CAPACITY: usize = 128;

/// Maximum length-prefixed frame size (4 MB).
pub const MAX_FRAME_SIZE: u32 = 4 * 1024 * 1024;

/// QUIC keepalive interval.
pub const KEEPALIVE_INTERVAL_SECS: u64 = 15;

/// Disconnect timeout (no keepalive received).
pub const DISCONNECT_TIMEOUT_SECS: u64 = 30;

/// Telemetry sensor sampling interval on camera (internal).
pub const TELEMETRY_SAMPLE_INTERVAL_SECS: u64 = 2;

/// Full telemetry heartbeat interval.
pub const TELEMETRY_HEARTBEAT_INTERVAL_SECS: u64 = 30;

/// Maximum telemetry datagrams buffered on camera for upload.
pub const TELEMETRY_BUFFER_CAP: usize = 100_000;

/// fMP4 segment duration.
pub const SEGMENT_DURATION_SECS: u64 = 6;

/// Segment coalescing buffer TTL.
pub const SEGMENT_BUFFER_TTL_SECS: u64 = 60;

/// Camera telemetry HTTP poll interval.
pub const TELEMETRY_POLL_INTERVAL_SECS: u64 = 10;

/// Presigned URL default TTL (seconds).
pub const PRESIGN_TTL_SECS: u64 = 3600;

/// Presigned URL batch size limit.
pub const PRESIGN_BATCH_MAX: u32 = 30;

/// Provision token default TTL (seconds). 24 hours.
pub const PROVISION_TOKEN_TTL_SECS: u64 = 24 * 60 * 60;

/// Camera considered offline after this many seconds without a telemetry poll.
pub const CAMERA_OFFLINE_THRESHOLD_SECS: u64 = 30;

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

/// Load and deserialize a TOML config file.
///
/// Returns `Ok(value)` on success, or an error with context describing the file path.
pub fn load_toml<T: serde::de::DeserializeOwned>(path: &Path) -> Result<T> {
    let contents =
        std::fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
    toml::from_str(&contents).with_context(|| format!("parsing {}", path.display()))
}

/// Read an environment variable, falling back to a default if unset or empty.
/// Logs a warning if the variable is set but cannot be parsed.
pub fn env_or<T: std::str::FromStr>(var: &str, default: T) -> T {
    match std::env::var(var) {
        Ok(s) if !s.is_empty() => match s.parse() {
            Ok(v) => v,
            Err(_) => {
                tracing::warn!(var, value = %s, "could not parse env var, using default");
                default
            }
        },
        _ => default,
    }
}

/// Read an optional environment variable. Returns `None` if unset or empty.
pub fn env_opt(var: &str) -> Option<String> {
    std::env::var(var).ok().filter(|s| !s.is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn load_toml_valid() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("test.toml");
        std::fs::write(&path, "value = 42\n").unwrap();

        #[derive(serde::Deserialize)]
        struct TestConf {
            value: u32,
        }

        let conf: TestConf = load_toml(&path).unwrap();
        assert_eq!(conf.value, 42);
    }

    #[test]
    fn load_toml_missing_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nonexistent.toml");

        #[derive(serde::Deserialize)]
        struct TestConf {
            #[allow(dead_code)]
            value: u32,
        }

        assert!(load_toml::<TestConf>(&path).is_err());
    }

    #[test]
    fn env_or_uses_default() {
        // Use a var name unlikely to be set
        let val: u16 = env_or("GHOSTCAM_TEST_NONEXISTENT_12345", 99);
        assert_eq!(val, 99);
    }
}
