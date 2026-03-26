use std::sync::atomic::{AtomicU64, Ordering};

/// Wraps a Redis `ConnectionManager` with health tracking.
///
/// `ConnectionManager` is `Clone`, handles automatic reconnection internally,
/// and multiplexes commands over a single connection — no manual reconnect
/// loop or `RwLock` needed.
pub struct RedisManager {
    conn: Option<redis::aio::ConnectionManager>,
    write_errors: AtomicU64,
}

impl RedisManager {
    /// Create a new manager. Attempts initial connection but does not fail
    /// if Redis is unavailable.
    pub async fn new(redis_url: &str) -> Self {
        match Self::try_connect(redis_url).await {
            Ok(conn) => {
                tracing::info!("redis connected");
                Self {
                    conn: Some(conn),
                    write_errors: AtomicU64::new(0),
                }
            }
            Err(e) => {
                tracing::warn!("redis initial connection failed: {e}");
                Self {
                    conn: None,
                    write_errors: AtomicU64::new(0),
                }
            }
        }
    }

    /// Get a connection clone. Returns None if Redis was not available at
    /// startup. `ConnectionManager` handles reconnection transparently —
    /// individual commands may fail transiently during a reconnect but will
    /// recover automatically.
    pub fn get_conn(&self) -> Option<redis::aio::ConnectionManager> {
        self.conn.clone()
    }

    /// Check if Redis was connected at startup.
    pub fn is_connected(&self) -> bool {
        self.conn.is_some()
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
        let manager = RedisManager::new("redis://invalid-host:99999").await;
        assert!(!manager.is_connected());
        assert!(manager.get_conn().is_none());
    }
}
