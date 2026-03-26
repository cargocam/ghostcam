use ghostcam::types::DeviceId;
use redis::AsyncCommands;

use super::connection::RedisManager;

const MANIFEST_KEY_PREFIX: &str = "manifest:";

/// Persist the camera's HLS manifest to Redis. Replaces any previous manifest.
/// No TTL — lifecycle is controlled by camera events and unregister.
pub async fn store_manifest(redis: &RedisManager, device_id: &DeviceId, manifest: &str) {
    let Some(mut conn) = redis.get_conn() else {
        tracing::debug!(device_id = %device_id, "redis unavailable — skipping manifest store");
        return;
    };
    let key = format!("{MANIFEST_KEY_PREFIX}{}", device_id.0);
    let _: Result<(), _> = conn.set::<_, _, ()>(&key, manifest).await;
}

/// Retrieve the camera's last-known manifest from Redis.
pub async fn get_manifest(redis: &RedisManager, device_id: &DeviceId) -> Option<String> {
    let mut conn = redis.get_conn()?;
    let key = format!("{MANIFEST_KEY_PREFIX}{}", device_id.0);
    conn.get(&key).await.ok()
}

/// Delete the manifest for a camera (called on unregister / purge).
pub async fn delete_manifest(redis: &RedisManager, device_id: &DeviceId) {
    let Some(mut conn) = redis.get_conn() else {
        return;
    };
    let key = format!("{MANIFEST_KEY_PREFIX}{}", device_id.0);
    let _: Result<(), _> = conn.del::<_, ()>(&key).await;
}
