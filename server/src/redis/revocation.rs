use std::sync::Arc;

use anyhow::Result;
use redis::AsyncCommands;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::connection::RedisManager;
use crate::pki::revocation::RevocationCache;

const REVOCATION_KEY: &str = "revoked_certs";

/// Spawn a background task that refreshes the revocation cache from Redis
/// every REVOCATION_CACHE_REFRESH_SECS (60s).
pub fn spawn_revocation_refresh(
    redis: Arc<RedisManager>,
    cache: Arc<RevocationCache>,
    cancel: CancellationToken,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        let interval =
            std::time::Duration::from_secs(ghostcam::config::REVOCATION_CACHE_REFRESH_SECS);

        // Initial load
        if let Err(e) = refresh_cache(&redis, &cache).await {
            tracing::debug!("initial revocation cache refresh failed: {e}");
        }

        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                _ = tokio::time::sleep(interval) => {
                    if let Err(e) = refresh_cache(&redis, &cache).await {
                        tracing::debug!("revocation cache refresh failed (retaining stale): {e}");
                    }
                }
            }
        }
    })
}

async fn refresh_cache(redis: &RedisManager, cache: &RevocationCache) -> Result<()> {
    let Some(mut conn) = redis.get_conn().await else {
        anyhow::bail!("redis unavailable");
    };

    let serials: Vec<String> = conn.smembers(REVOCATION_KEY).await?;
    for serial in serials {
        cache.add(serial).await;
    }
    Ok(())
}

/// Add a serial number to the Redis revocation set.
pub async fn revoke_cert(redis: &RedisManager, serial: &str) -> Result<()> {
    let Some(mut conn) = redis.get_conn().await else {
        anyhow::bail!("redis unavailable for revocation — this is critical");
    };

    conn.sadd::<_, _, ()>(REVOCATION_KEY, serial).await?;
    Ok(())
}
