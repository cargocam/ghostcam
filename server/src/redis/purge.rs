use ghostcam::types::DeviceId;
use redis::AsyncCommands;

use super::connection::RedisManager;
use super::segments;

const TELEMETRY_KEY_PREFIX: &str = "telemetry:";

/// Purge all Redis data for a device: telemetry stream + segment metadata.
pub async fn purge_device_data(redis: &RedisManager, device_id: &DeviceId) {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, "redis unavailable — skipping purge");
        return;
    };

    // Delete telemetry stream
    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);
    let _: Result<(), _> = conn.del::<_, ()>(&key).await;

    // Delete all segment keys
    segments::purge_segments(redis, device_id).await;
}
