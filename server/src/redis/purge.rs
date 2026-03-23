use ghostcam::types::DeviceId;
use redis::AsyncCommands;

use super::connection::RedisManager;

const TELEMETRY_KEY_PREFIX: &str = "telemetry:";

/// Purge all Redis data for a device: telemetry stream and manifest.
pub async fn purge_device_data(redis: &RedisManager, device_id: &DeviceId) {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, "redis unavailable — skipping purge");
        return;
    };

    let telemetry_key = format!("{TELEMETRY_KEY_PREFIX}{}", device_id.0);
    let _: Result<(), _> = conn.del::<_, ()>(&telemetry_key).await;

    super::manifest::delete_manifest(redis, device_id).await;
}
