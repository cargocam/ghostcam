use std::collections::{HashMap, HashSet};

use ghostcam::types::{DeviceId, UserId};
use tokio::sync::RwLock;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

struct SessionEntry {
    device_id: DeviceId,
    user_id: UserId,
    cancel: CancellationToken,
    handle: JoinHandle<()>,
}

/// Tracks active WebRTC sessions for teardown and scoping.
///
/// A reverse index (`by_device`) enables O(1) lookup of all sessions for a
/// given camera, avoiding a full table scan on camera disconnect.
pub struct SessionManager {
    sessions: RwLock<SessionInner>,
}

struct SessionInner {
    by_id: HashMap<String, SessionEntry>,
    /// Reverse index: DeviceId → set of session IDs.
    by_device: HashMap<DeviceId, HashSet<String>>,
    /// Reverse index: UserId → set of session IDs (for per-user limits).
    by_user: HashMap<UserId, HashSet<String>>,
}

impl SessionManager {
    pub fn new() -> Self {
        Self {
            sessions: RwLock::new(SessionInner {
                by_id: HashMap::new(),
                by_device: HashMap::new(),
                by_user: HashMap::new(),
            }),
        }
    }

    /// Register a new session. The provided task handle is the egress event loop.
    /// Returns `false` if the per-user session limit is exceeded (caller should
    /// return 429 and clean up the egress handle).
    pub async fn register(
        &self,
        session_id: String,
        device_id: DeviceId,
        user_id: UserId,
        cancel: CancellationToken,
        handle: JoinHandle<()>,
    ) -> bool {
        let mut inner = self.sessions.write().await;

        // Enforce per-user session limit under the write lock to avoid TOCTOU races.
        let user_count = inner.by_user.get(&user_id).map(|s| s.len()).unwrap_or(0);
        if user_count >= ghostcam::config::MAX_SESSIONS_PER_USER {
            return false;
        }

        inner
            .by_device
            .entry(device_id.clone())
            .or_default()
            .insert(session_id.clone());
        inner
            .by_user
            .entry(user_id.clone())
            .or_default()
            .insert(session_id.clone());
        inner.by_id.insert(
            session_id,
            SessionEntry {
                device_id,
                user_id,
                cancel,
                handle,
            },
        );
        true
    }

    /// Tear down a session by ID.
    pub async fn teardown(&self, session_id: &str) -> bool {
        let mut inner = self.sessions.write().await;
        if let Some(entry) = inner.by_id.remove(session_id) {
            // Remove from device reverse index.
            if let Some(ids) = inner.by_device.get_mut(&entry.device_id) {
                ids.remove(session_id);
                if ids.is_empty() {
                    inner.by_device.remove(&entry.device_id);
                }
            }
            // Remove from user reverse index.
            if let Some(ids) = inner.by_user.get_mut(&entry.user_id) {
                ids.remove(session_id);
                if ids.is_empty() {
                    inner.by_user.remove(&entry.user_id);
                }
            }
            entry.cancel.cancel();
            entry.handle.abort();
            true
        } else {
            false
        }
    }

    /// Tear down all sessions for a device (called on camera disconnect).
    pub async fn teardown_by_device(&self, device_id: &DeviceId) {
        let mut inner = self.sessions.write().await;
        if let Some(session_ids) = inner.by_device.remove(device_id) {
            for key in session_ids {
                if let Some(entry) = inner.by_id.remove(&key) {
                    // Remove from user reverse index.
                    if let Some(ids) = inner.by_user.get_mut(&entry.user_id) {
                        ids.remove(&key);
                        if ids.is_empty() {
                            inner.by_user.remove(&entry.user_id);
                        }
                    }
                    entry.cancel.cancel();
                    entry.handle.abort();
                }
            }
        }
    }

    /// Get the user_id for a session.
    pub async fn get_user_id(&self, session_id: &str) -> Option<UserId> {
        let inner = self.sessions.read().await;
        inner.by_id.get(session_id).map(|e| e.user_id.clone())
    }
}

impl Default for SessionManager {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spawn_dummy() -> (CancellationToken, JoinHandle<()>) {
        let cancel = CancellationToken::new();
        let c = cancel.clone();
        let handle = tokio::spawn(async move {
            c.cancelled().await;
        });
        (cancel, handle)
    }

    #[tokio::test]
    async fn register_and_get_user() {
        let mgr = SessionManager::new();
        let (cancel, handle) = spawn_dummy();
        let uid = UserId::from("user-1");
        mgr.register(
            "s1".into(),
            DeviceId::from("cam-1"),
            uid.clone(),
            cancel,
            handle,
        )
        .await;
        let found = mgr.get_user_id("s1").await;
        assert_eq!(found, Some(uid));
    }

    #[tokio::test]
    async fn teardown_removes() {
        let mgr = SessionManager::new();
        let (cancel, handle) = spawn_dummy();
        let uid = UserId::from("user-1");
        mgr.register(
            "s1".into(),
            DeviceId::from("cam-1"),
            uid.clone(),
            cancel,
            handle,
        )
        .await;
        assert!(mgr.teardown("s1").await);
        assert!(mgr.get_user_id("s1").await.is_none());
    }

    #[tokio::test]
    async fn teardown_by_device() {
        let mgr = SessionManager::new();
        let uid = UserId::from("user-1");

        for i in 0..2 {
            let (cancel, handle) = spawn_dummy();
            mgr.register(
                format!("sa-{i}"),
                DeviceId::from("cam-a"),
                uid.clone(),
                cancel,
                handle,
            )
            .await;
        }
        {
            let (cancel, handle) = spawn_dummy();
            mgr.register(
                "sb-0".into(),
                DeviceId::from("cam-b"),
                uid.clone(),
                cancel,
                handle,
            )
            .await;
        }

        mgr.teardown_by_device(&DeviceId::from("cam-a")).await;
        // sb-0 should still exist
        assert!(mgr.get_user_id("sb-0").await.is_some());
        // sa-0 and sa-1 should be gone
        assert!(mgr.get_user_id("sa-0").await.is_none());
        assert!(mgr.get_user_id("sa-1").await.is_none());
    }

    #[tokio::test]
    async fn teardown_nonexistent() {
        let mgr = SessionManager::new();
        assert!(!mgr.teardown("nonexistent").await);
    }
}
