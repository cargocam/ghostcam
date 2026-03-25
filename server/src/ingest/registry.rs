use std::collections::HashMap;
use std::sync::Arc;

use ghostcam::types::{DeviceId, UserId};
use tokio::sync::RwLock;

use super::slot::IngestSlot;

/// Registry of all connected cameras, keyed by owning user then device.
///
/// A flat secondary index (`by_device`) provides O(1) lookups by `DeviceId`,
/// which is the common path for WebRTC session creation and segment requests.
pub struct RoutingRegistry {
    cameras: RwLock<RegistryInner>,
}

struct RegistryInner {
    /// Nested map for user-scoped queries.
    by_user: HashMap<UserId, HashMap<DeviceId, Arc<IngestSlot>>>,
    /// Flat index for O(1) device lookups.
    by_device: HashMap<DeviceId, Arc<IngestSlot>>,
}

impl RoutingRegistry {
    pub fn new() -> Self {
        Self {
            cameras: RwLock::new(RegistryInner {
                by_user: HashMap::new(),
                by_device: HashMap::new(),
            }),
        }
    }

    /// Register an IngestSlot. If a stale slot exists for the same device_id,
    /// it is shut down and replaced.
    pub async fn register(&self, slot: Arc<IngestSlot>) {
        let mut inner = self.cameras.write().await;
        let user_map = inner.by_user.entry(slot.user_id.clone()).or_default();

        if let Some(old_slot) = user_map.insert(slot.device_id.clone(), slot.clone()) {
            old_slot.shutdown();
            tracing::info!(
                device_id = %slot.device_id,
                "replaced stale slot"
            );
        }
        inner.by_device.insert(slot.device_id.clone(), slot);
    }

    /// Remove an IngestSlot on camera disconnect.
    /// Only removes if the registered slot is the exact same Arc instance,
    /// preventing a stale connection's cleanup from removing a newer slot.
    pub async fn unregister(&self, device_id: &DeviceId, slot: &Arc<IngestSlot>) {
        let mut inner = self.cameras.write().await;
        // Check the flat index first for O(1) identity verification.
        if let Some(existing) = inner.by_device.get(device_id) {
            if Arc::ptr_eq(existing, slot) {
                inner.by_device.remove(device_id);
                // O(1): use the slot's own user_id to target the right bucket.
                if let Some(user_map) = inner.by_user.get_mut(&slot.user_id) {
                    user_map.remove(device_id);
                    if user_map.is_empty() {
                        inner.by_user.remove(&slot.user_id);
                    }
                }
            }
        }
    }

    /// Look up a slot by device_id. Returns None if camera is not connected.
    pub async fn get_slot(&self, device_id: &DeviceId) -> Option<Arc<IngestSlot>> {
        let inner = self.cameras.read().await;
        inner.by_device.get(device_id).cloned()
    }

    /// Check if a device is currently connected.
    pub async fn is_connected(&self, device_id: &DeviceId) -> bool {
        let inner = self.cameras.read().await;
        inner.by_device.contains_key(device_id)
    }
}

impl Default for RoutingRegistry {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ingest::slot::test_slot;

    #[tokio::test]
    async fn register_and_get() {
        let registry = RoutingRegistry::new();
        let slot = test_slot("cam-1", "user-1");
        registry.register(slot.clone()).await;
        let found = registry.get_slot(&DeviceId("cam-1".into())).await;
        assert!(found.is_some());
    }

    #[tokio::test]
    async fn get_unregistered() {
        let registry = RoutingRegistry::new();
        let found = registry.get_slot(&DeviceId("cam-1".into())).await;
        assert!(found.is_none());
    }

    #[tokio::test]
    async fn unregister() {
        let registry = RoutingRegistry::new();
        let slot = test_slot("cam-1", "user-1");
        registry.register(slot.clone()).await;
        registry.unregister(&DeviceId("cam-1".into()), &slot).await;
        assert!(!registry.is_connected(&DeviceId("cam-1".into())).await);
    }

    #[tokio::test]
    async fn is_connected() {
        let registry = RoutingRegistry::new();
        let slot = test_slot("cam-1", "user-1");
        assert!(!registry.is_connected(&DeviceId("cam-1".into())).await);
        registry.register(slot.clone()).await;
        assert!(registry.is_connected(&DeviceId("cam-1".into())).await);
        registry.unregister(&DeviceId("cam-1".into()), &slot).await;
        assert!(!registry.is_connected(&DeviceId("cam-1".into())).await);
    }

    #[tokio::test]
    async fn replace_stale_slot() {
        let registry = RoutingRegistry::new();
        let old_slot = test_slot("cam-1", "user-1");
        registry.register(old_slot.clone()).await;

        let new_slot = test_slot("cam-1", "user-1");
        registry.register(new_slot.clone()).await;

        // Old slot should be shut down
        assert!(old_slot.is_shutdown());

        // New slot is returned by get
        let found = registry.get_slot(&DeviceId("cam-1".into())).await.unwrap();
        assert!(!found.is_shutdown());
    }

    #[tokio::test]
    async fn concurrent_register_unregister() {
        let registry = Arc::new(RoutingRegistry::new());
        let mut handles = Vec::new();

        for i in 0..10 {
            let reg = registry.clone();
            handles.push(tokio::spawn(async move {
                let slot = test_slot(&format!("cam-{i}"), "user-1");
                reg.register(slot.clone()).await;
                tokio::task::yield_now().await;
                reg.unregister(&DeviceId(format!("cam-{i}")), &slot).await;
            }));
        }

        for h in handles {
            h.await.unwrap();
        }

        // All should be unregistered
        for i in 0..10 {
            assert!(!registry.is_connected(&DeviceId(format!("cam-{i}"))).await);
        }
    }

    #[tokio::test]
    async fn stale_unregister_does_not_remove_new_slot() {
        let registry = RoutingRegistry::new();
        let old_slot = test_slot("cam-1", "user-1");
        registry.register(old_slot.clone()).await;

        // New connection replaces old slot
        let new_slot = test_slot("cam-1", "user-1");
        registry.register(new_slot.clone()).await;

        // Old connection's cleanup tries to unregister — should be a no-op
        registry
            .unregister(&DeviceId("cam-1".into()), &old_slot)
            .await;

        // New slot should still be registered
        assert!(registry.is_connected(&DeviceId("cam-1".into())).await);
        let found = registry.get_slot(&DeviceId("cam-1".into())).await.unwrap();
        assert!(Arc::ptr_eq(&found, &new_slot));
    }
}
