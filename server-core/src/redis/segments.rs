use anyhow::Result;
use ghostcam::types::DeviceId;
use redis::AsyncCommands;
use serde::{Deserialize, Serialize};

use super::connection::RedisManager;

const SEGMENT_KEY_PREFIX: &str = "segments:";
const SEGMENT_TTL_SECS: u64 = 72 * 60 * 60; // 72 hours

/// Segment metadata stored in Redis.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SegmentMetadata {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
}

/// Upsert segment metadata to Redis.
pub async fn upsert_segment(
    redis: &RedisManager,
    device_id: &DeviceId,
    segment_id: &str,
    start_ts: u64,
    end_ts: u64,
    size_bytes: u64,
) {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, segment_id, "redis unavailable — dropping segment upsert");
        return;
    };

    let key = format!("{}{}:{}", SEGMENT_KEY_PREFIX, device_id.0, segment_id);

    let result: Result<(), redis::RedisError> = redis::pipe()
        .hset_multiple(
            &key,
            &[
                ("segment_id", segment_id.to_string()),
                ("start_ts", start_ts.to_string()),
                ("end_ts", end_ts.to_string()),
                ("size_bytes", size_bytes.to_string()),
            ],
        )
        .expire(&key, SEGMENT_TTL_SECS as i64)
        .query_async(&mut conn)
        .await;

    if let Err(e) = result {
        redis.record_write_error();
        tracing::debug!(device_id = %device_id, segment_id, "redis segment upsert error: {e}");
    }
}

/// Tombstone a segment (delete the key).
pub async fn delete_segment(redis: &RedisManager, device_id: &DeviceId, segment_id: &str) {
    let Some(mut conn) = redis.get_conn().await else {
        return;
    };

    let key = format!("{}{}:{}", SEGMENT_KEY_PREFIX, device_id.0, segment_id);
    let _: Result<(), _> = conn.del::<_, ()>(&key).await;
}

/// List all segment metadata for a camera, sorted by start_ts.
pub async fn list_segments(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<Vec<SegmentMetadata>> {
    let Some(mut conn) = redis.get_conn().await else {
        anyhow::bail!("redis unavailable");
    };

    let pattern = format!("{}{}:*", SEGMENT_KEY_PREFIX, device_id.0);

    // Use SCAN to find matching keys
    let keys: Vec<String> = redis::cmd("KEYS")
        .arg(&pattern)
        .query_async(&mut conn)
        .await?;

    let mut segments = Vec::new();
    for key in &keys {
        let values: Vec<(String, String)> = conn.hgetall(key).await?;
        if values.is_empty() {
            continue;
        }

        let mut seg = SegmentMetadata {
            segment_id: String::new(),
            start_ts: 0,
            end_ts: 0,
            size_bytes: 0,
        };

        for (k, v) in &values {
            match k.as_str() {
                "segment_id" => seg.segment_id = v.clone(),
                "start_ts" => seg.start_ts = v.parse().unwrap_or(0),
                "end_ts" => seg.end_ts = v.parse().unwrap_or(0),
                "size_bytes" => seg.size_bytes = v.parse().unwrap_or(0),
                _ => {}
            }
        }

        if !seg.segment_id.is_empty() {
            segments.push(seg);
        }
    }

    segments.sort_by_key(|s| s.start_ts);
    Ok(segments)
}

/// Delete all segment metadata for a camera.
pub async fn purge_segments(redis: &RedisManager, device_id: &DeviceId) {
    let Some(mut conn) = redis.get_conn().await else {
        return;
    };

    let pattern = format!("{}{}:*", SEGMENT_KEY_PREFIX, device_id.0);
    let keys: Result<Vec<String>, _> = redis::cmd("KEYS")
        .arg(&pattern)
        .query_async(&mut conn)
        .await;

    if let Ok(keys) = keys {
        for key in &keys {
            let _: Result<(), _> = conn.del(key).await;
        }
    }
}
