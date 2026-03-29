use std::sync::Arc;

use redis::AsyncCommands;

use super::connection::RedisManager;
use crate::api::state::AppState;

/// Redis pub/sub channel name for firmware release notifications.
const FIRMWARE_CHANNEL: &str = "ghostcam:firmware:release";

/// Publish a firmware release notification to Redis pub/sub.
pub async fn publish_release(redis: &Arc<RedisManager>, json: &str) {
    let Some(mut conn) = redis.get_conn() else {
        tracing::warn!("redis not connected — cannot publish firmware release");
        return;
    };

    if let Err(e) = conn.publish::<_, _, ()>(FIRMWARE_CHANNEL, json).await {
        tracing::warn!("failed to publish firmware release to Redis: {e}");
        redis.record_write_error();
    } else {
        tracing::debug!("published firmware release to Redis pub/sub");
    }
}

/// Subscribe to firmware release notifications via a dedicated Redis connection.
/// Runs forever (until cancel).
pub async fn subscribe_firmware_releases(
    redis_url: &str,
    state: Arc<AppState>,
    cancel: tokio_util::sync::CancellationToken,
) {
    loop {
        if cancel.is_cancelled() {
            return;
        }

        let client = match redis::Client::open(redis_url) {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!("failed to create Redis client for pub/sub: {e}");
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => continue,
                }
            }
        };

        let mut pubsub = match client.get_async_pubsub().await {
            Ok(ps) => ps,
            Err(e) => {
                tracing::warn!("failed to connect to Redis for pub/sub: {e}");
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => continue,
                }
            }
        };
        if let Err(e) = pubsub.subscribe(FIRMWARE_CHANNEL).await {
            tracing::warn!("failed to subscribe to firmware channel: {e}");
            tokio::select! {
                _ = cancel.cancelled() => return,
                _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => continue,
            }
        }

        tracing::info!("subscribed to Redis firmware release channel");

        let mut msg_stream = pubsub.on_message();

        loop {
            use tokio_stream::StreamExt;
            tokio::select! {
                _ = cancel.cancelled() => return,
                msg = msg_stream.next() => {
                    let Some(msg) = msg else { break };
                    let payload: String = match msg.get_payload() {
                        Ok(p) => p,
                        Err(e) => {
                            tracing::warn!("firmware pub/sub payload error: {e}");
                            continue;
                        }
                    };

                    match serde_json::from_str::<ghostcam::firmware::FirmwareRelease>(&payload) {
                        Ok(release) => {
                            tracing::info!(version = %release.version, "firmware release from Redis pub/sub");
                            *state.firmware_release.write().await = Some(release);
                            crate::firmware::schedule_staggered_reboot(&state);
                        }
                        Err(e) => {
                            tracing::warn!("firmware pub/sub JSON parse error: {e}");
                        }
                    }
                }
            }
        }

        tracing::warn!("firmware pub/sub connection lost — reconnecting");
    }
}
