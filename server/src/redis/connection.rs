use std::sync::atomic::{AtomicU64, Ordering};
use tokio::sync::RwLock;

/// Wraps a Redis `ConnectionManager` with health tracking.
///
/// `ConnectionManager` is `Clone` and handles automatic reconnection once
/// established. If Redis is unavailable at startup, a background retry loop
/// will keep trying until a connection is established.
pub struct RedisManager {
    conn: RwLock<Option<redis::aio::ConnectionManager>>,
    write_errors: AtomicU64,
}

impl RedisManager {
    /// Create a new manager. Attempts initial connection but does not fail
    /// if Redis is unavailable — a background task retries until connected.
    pub async fn new(
        redis_url: &str,
        cancel: tokio_util::sync::CancellationToken,
    ) -> std::sync::Arc<Self> {
        let mgr = std::sync::Arc::new(Self {
            conn: RwLock::new(None),
            write_errors: AtomicU64::new(0),
        });

        match Self::try_connect(redis_url).await {
            Ok(conn) => {
                tracing::info!("redis connected");
                *mgr.conn.write().await = Some(conn);
            }
            Err(e) => {
                tracing::warn!("redis initial connection failed: {e} — retrying in background");
                let mgr2 = mgr.clone();
                let url = redis_url.to_string();
                tokio::spawn(async move {
                    let mut delay = std::time::Duration::from_secs(1);
                    let max_delay = std::time::Duration::from_secs(30);
                    loop {
                        tokio::select! {
                            _ = cancel.cancelled() => break,
                            _ = tokio::time::sleep(delay) => {}
                        }
                        match Self::try_connect(&url).await {
                            Ok(conn) => {
                                tracing::info!("redis connected (background retry)");
                                *mgr2.conn.write().await = Some(conn);
                                break;
                            }
                            Err(e) => {
                                tracing::debug!("redis retry failed: {e}");
                                delay = (delay * 2).min(max_delay);
                            }
                        }
                    }
                });
            }
        }

        mgr
    }

    /// Get a connection clone. Returns None if Redis is not yet connected.
    /// `ConnectionManager` handles reconnection transparently once established —
    /// individual commands may fail transiently during a reconnect but will
    /// recover automatically.
    pub fn get_conn(&self) -> Option<redis::aio::ConnectionManager> {
        self.conn.try_read().ok().and_then(|g| g.clone())
    }

    /// Check if Redis is connected.
    pub fn is_connected(&self) -> bool {
        self.conn.try_read().map(|g| g.is_some()).unwrap_or(false)
    }

    /// Increment the write error counter.
    pub fn record_write_error(&self) {
        self.write_errors.fetch_add(1, Ordering::Relaxed);
    }

    async fn try_connect(
        redis_url: &str,
    ) -> Result<redis::aio::ConnectionManager, redis::RedisError> {
        let client = redis::Client::open(redis_url)?;
        client.get_connection_manager().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn new_without_redis() {
        let cancel = tokio_util::sync::CancellationToken::new();
        let manager = RedisManager::new("redis://invalid-host:99999", cancel.clone()).await;
        assert!(!manager.is_connected());
        assert!(manager.get_conn().is_none());
        cancel.cancel();
    }
}
