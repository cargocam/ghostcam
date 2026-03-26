use std::sync::Arc;

use ghostcam::types::DeviceId;
use redis::AsyncCommands;
use tokio_util::sync::CancellationToken;

use super::connection::RedisManager;

const TELEMETRY_KEY_PREFIX: &str = "telemetry:";

/// How often the background purge task runs (1 hour).
const PURGE_INTERVAL_SECS: u64 = 60 * 60;

/// Purge all Redis data for a device: telemetry stream and manifest.
/// Called when a device is unregistered.
pub async fn purge_device_data(redis: &RedisManager, device_id: &DeviceId) {
    let Some(mut conn) = redis.get_conn() else {
        tracing::debug!(device_id = %device_id, "redis unavailable — skipping purge");
        return;
    };

    let telemetry_key = format!("{TELEMETRY_KEY_PREFIX}{}", device_id.0);
    let _: Result<(), _> = conn.del::<_, ()>(&telemetry_key).await;

    super::manifest::delete_manifest(redis, device_id).await;
}

/// Trim all `telemetry:*` streams to remove entries older than the retention window.
///
/// Uses `SCAN` to discover all telemetry stream keys, then issues `XTRIM MINID ~`
/// on each to evict stale entries. This covers cameras that have gone offline and
/// are no longer writing (where the inline MINID trim on XADD would not fire).
pub async fn purge_old_telemetry(redis: &RedisManager) {
    let Some(mut conn) = redis.get_conn() else {
        tracing::debug!("redis unavailable — skipping telemetry purge");
        return;
    };

    let retention_ms = ghostcam::config::TELEMETRY_RETENTION_SECS * 1000;
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64;
    let min_id = now_ms.saturating_sub(retention_ms);

    // Scan for all telemetry stream keys
    let pattern = format!("{TELEMETRY_KEY_PREFIX}*");
    let keys: Vec<String> = match redis::cmd("KEYS")
        .arg(&pattern)
        .query_async::<Vec<String>>(&mut conn)
        .await
    {
        Ok(keys) => keys,
        Err(e) => {
            tracing::warn!("failed to list telemetry keys for purge: {e}");
            return;
        }
    };

    if keys.is_empty() {
        return;
    }

    let mut trimmed = 0u64;
    for key in &keys {
        let result: Result<u64, redis::RedisError> = redis::cmd("XTRIM")
            .arg(key)
            .arg("MINID")
            .arg("~")
            .arg(min_id)
            .query_async(&mut conn)
            .await;

        match result {
            Ok(n) if n > 0 => trimmed += n,
            Err(e) => {
                tracing::debug!(key = %key, "xtrim error during purge: {e}");
            }
            _ => {}
        }
    }

    if trimmed > 0 {
        tracing::info!(
            streams = keys.len(),
            entries_trimmed = trimmed,
            "telemetry purge complete"
        );
    } else {
        tracing::debug!(streams = keys.len(), "telemetry purge: nothing to trim");
    }
}

/// Spawn a background task that periodically trims old telemetry entries.
///
/// Runs `purge_old_telemetry()` once every hour. The task exits when the
/// cancellation token is triggered during shutdown.
pub fn spawn_telemetry_purge(redis: Arc<RedisManager>, cancel: CancellationToken) {
    tokio::spawn(async move {
        let interval = tokio::time::Duration::from_secs(PURGE_INTERVAL_SECS);
        // Run once at startup after a short delay, then hourly.
        tokio::time::sleep(tokio::time::Duration::from_secs(30)).await;
        loop {
            purge_old_telemetry(&redis).await;

            tokio::select! {
                _ = tokio::time::sleep(interval) => {}
                _ = cancel.cancelled() => {
                    tracing::debug!("telemetry purge task shutting down");
                    return;
                }
            }
        }
    });
}
