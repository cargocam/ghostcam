use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;

use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;

/// Wraps a Redis connection with automatic reconnection and health tracking.
pub struct RedisManager {
    conn: RwLock<Option<redis::aio::MultiplexedConnection>>,
    url: String,
    write_errors: AtomicU64,
    connected: AtomicBool,
}

impl RedisManager {
    /// Create a new manager. Attempts initial connection but does not fail
    /// if Redis is unavailable.
    pub async fn new(redis_url: &str) -> Self {
        let manager = Self {
            conn: RwLock::new(None),
            url: redis_url.to_string(),
            write_errors: AtomicU64::new(0),
            connected: AtomicBool::new(false),
        };

        match manager.try_connect().await {
            Ok(conn) => {
                *manager.conn.write().await = Some(conn);
                manager.connected.store(true, Ordering::SeqCst);
                tracing::info!("redis connected");
            }
            Err(e) => {
                tracing::warn!("redis initial connection failed (will retry): {e}");
            }
        }

        manager
    }

    /// Get a connection clone. Returns None if Redis is unavailable.
    pub async fn get_conn(&self) -> Option<redis::aio::MultiplexedConnection> {
        self.conn.read().await.clone()
    }

    /// Check if Redis is currently connected.
    pub fn is_connected(&self) -> bool {
        self.connected.load(Ordering::SeqCst)
    }

    /// Increment the write error counter.
    pub fn record_write_error(&self) {
        self.write_errors.fetch_add(1, Ordering::SeqCst);
    }

    /// Spawn a background reconnect loop.
    pub fn spawn_reconnect_loop(self: &Arc<Self>, cancel: CancellationToken) {
        let manager = self.clone();
        tokio::spawn(async move {
            let mut backoff_secs = ghostcam::config::RECONNECT_BACKOFF_INITIAL_SECS;

            loop {
                tokio::select! {
                    _ = cancel.cancelled() => break,
                    _ = tokio::time::sleep(std::time::Duration::from_secs(backoff_secs)) => {}
                }

                if manager.is_connected() {
                    // Check if connection is still alive
                    let alive = {
                        if let Some(mut conn) = manager.get_conn().await {
                            redis::cmd("PING")
                                .query_async::<String>(&mut conn)
                                .await
                                .is_ok()
                        } else {
                            false
                        }
                    };

                    if alive {
                        backoff_secs = ghostcam::config::RECONNECT_BACKOFF_INITIAL_SECS;
                        continue;
                    }

                    // Connection lost
                    manager.connected.store(false, Ordering::SeqCst);
                    *manager.conn.write().await = None;
                    tracing::warn!("redis connection lost, reconnecting...");
                }

                match manager.try_connect().await {
                    Ok(conn) => {
                        *manager.conn.write().await = Some(conn);
                        manager.connected.store(true, Ordering::SeqCst);
                        backoff_secs = ghostcam::config::RECONNECT_BACKOFF_INITIAL_SECS;
                        tracing::info!("redis reconnected");
                    }
                    Err(e) => {
                        tracing::debug!("redis reconnect failed: {e}");
                        backoff_secs =
                            (backoff_secs * 2).min(ghostcam::config::RECONNECT_BACKOFF_MAX_SECS);
                    }
                }
            }
        });
    }

    async fn try_connect(&self) -> Result<redis::aio::MultiplexedConnection, redis::RedisError> {
        let client = redis::Client::open(self.url.as_str())?;
        client.get_multiplexed_tokio_connection().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn new_without_redis() {
        let manager = RedisManager::new("redis://invalid-host:99999").await;
        assert!(!manager.is_connected());
        assert!(manager.get_conn().await.is_none());
    }

}
