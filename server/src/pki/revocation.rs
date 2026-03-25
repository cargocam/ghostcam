use std::collections::HashSet;

use tokio::sync::RwLock;

/// In-memory set of revoked certificate serial numbers.
/// Populated from Redis on startup and refreshed every 60s by
/// `redis::revocation::spawn_revocation_refresh()`.
///
/// This cache only grows — serials are never removed. Un-revocation
/// requires a server restart. This is intentionally fail-safe: a revoked
/// camera stays revoked even if the Redis set is modified.
pub struct RevocationCache {
    revoked: RwLock<HashSet<String>>,
}

impl Default for RevocationCache {
    fn default() -> Self {
        Self::new()
    }
}

impl RevocationCache {
    pub fn new() -> Self {
        Self {
            revoked: RwLock::new(HashSet::new()),
        }
    }

    /// Check if a certificate serial number is revoked.
    pub async fn is_revoked(&self, serial: &str) -> bool {
        self.revoked.read().await.contains(serial)
    }

    /// Add a single serial number (called on unregistration).
    pub async fn add(&self, serial: String) {
        self.revoked.write().await.insert(serial);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn empty_cache_not_revoked() {
        let cache = RevocationCache::new();
        assert!(!cache.is_revoked("anything").await);
    }

    #[tokio::test]
    async fn add_and_check() {
        let cache = RevocationCache::new();
        cache.add("abc".to_string()).await;
        assert!(cache.is_revoked("abc").await);
    }

    #[tokio::test]
    async fn add_does_not_affect_others() {
        let cache = RevocationCache::new();
        cache.add("abc".to_string()).await;
        assert!(!cache.is_revoked("def").await);
    }


    #[tokio::test]
    async fn concurrent_reads() {
        let cache = std::sync::Arc::new(RevocationCache::new());
        cache.add("serial-1".to_string()).await;

        let mut handles = vec![];
        for _ in 0..100 {
            let c = cache.clone();
            handles.push(tokio::spawn(async move {
                let _ = c.is_revoked("serial-1").await;
            }));
        }
        for h in handles {
            h.await.unwrap();
        }
    }

    #[tokio::test]
    async fn concurrent_add_and_read() {
        let cache = std::sync::Arc::new(RevocationCache::new());
        let mut handles = vec![];

        for i in 0..50 {
            let c = cache.clone();
            handles.push(tokio::spawn(async move {
                c.add(format!("serial-{i}")).await;
            }));
        }
        for i in 0..50 {
            let c = cache.clone();
            handles.push(tokio::spawn(async move {
                let _ = c.is_revoked(&format!("serial-{i}")).await;
            }));
        }
        for h in handles {
            h.await.unwrap();
        }
    }
}
