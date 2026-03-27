#![allow(dead_code)]

use std::sync::Arc;

use redis::AsyncCommands;

use super::connection::RedisManager;

/// Redis key for monthly bandwidth counter: `usage:bw:{user_id}:{YYYY-MM}`
fn bandwidth_key(user_id: &str, month: &str) -> String {
    format!("usage:bw:{user_id}:{month}")
}

/// Redis key for monthly storage counter: `usage:st:{user_id}:{YYYY-MM}`
fn storage_key(user_id: &str, month: &str) -> String {
    format!("usage:st:{user_id}:{month}")
}

/// TTL for usage keys: 90 days.
const USAGE_TTL_SECS: u64 = 90 * 86400;

/// Increment the bandwidth counter for a user's current month.
pub async fn increment_bandwidth(
    redis: &Arc<RedisManager>,
    user_id: &str,
    month: &str,
    bytes: i64,
) {
    let Some(mut conn) = redis.get_conn() else {
        return;
    };
    let key = bandwidth_key(user_id, month);
    let result: Result<i64, _> = conn.incr(&key, bytes).await;
    match result {
        Ok(_) => {
            let _: Result<(), _> = conn.expire(&key, USAGE_TTL_SECS as i64).await;
        }
        Err(e) => {
            redis.record_write_error();
            tracing::debug!("redis bandwidth increment failed: {e}");
        }
    }
}

/// Increment the storage counter for a user's current month.
pub async fn increment_storage(redis: &Arc<RedisManager>, user_id: &str, month: &str, bytes: i64) {
    let Some(mut conn) = redis.get_conn() else {
        return;
    };
    let key = storage_key(user_id, month);
    let result: Result<i64, _> = conn.incr(&key, bytes).await;
    match result {
        Ok(_) => {
            let _: Result<(), _> = conn.expire(&key, USAGE_TTL_SECS as i64).await;
        }
        Err(e) => {
            redis.record_write_error();
            tracing::debug!("redis storage increment failed: {e}");
        }
    }
}

/// Read the current bandwidth and storage counters for a user's month.
/// Returns (bandwidth_bytes, storage_bytes). Returns (0, 0) if Redis is unavailable.
pub async fn get_usage(redis: &Arc<RedisManager>, user_id: &str, month: &str) -> (i64, i64) {
    let Some(mut conn) = redis.get_conn() else {
        return (0, 0);
    };
    let bw_key = bandwidth_key(user_id, month);
    let st_key = storage_key(user_id, month);

    let bw: i64 = conn.get(&bw_key).await.unwrap_or(0);
    let st: i64 = conn.get(&st_key).await.unwrap_or(0);
    (bw, st)
}
