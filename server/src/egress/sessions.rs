use std::collections::HashMap;

use ghostcam::config::{MAX_SESSIONS_PER_USER, MAX_VIEWERS_PER_CAMERA};
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
pub struct SessionManager {
    sessions: RwLock<HashMap<String, SessionEntry>>,
}

impl SessionManager {
    pub fn new() -> Self {
        Self {
            sessions: RwLock::new(HashMap::new()),
        }
    }

    /// Register a new session. The provided task handle is the egress event loop.
    pub async fn register(
        &self,
        session_id: String,
        device_id: DeviceId,
        user_id: UserId,
        cancel: CancellationToken,
        handle: JoinHandle<()>,
    ) {
        let mut sessions = self.sessions.write().await;
        sessions.insert(
            session_id,
            SessionEntry {
                device_id,
                user_id,
                cancel,
                handle,
            },
        );
    }

    /// Tear down a session by ID.
    pub async fn teardown(&self, session_id: &str) -> bool {
        let mut sessions = self.sessions.write().await;
        if let Some(entry) = sessions.remove(session_id) {
            entry.cancel.cancel();
            entry.handle.abort();
            true
        } else {
            false
        }
    }

    /// Tear down all sessions for a device (called on camera disconnect).
    pub async fn teardown_by_device(&self, device_id: &DeviceId) {
        let mut sessions = self.sessions.write().await;
        let to_remove: Vec<String> = sessions
            .iter()
            .filter(|(_, e)| &e.device_id == device_id)
            .map(|(k, _)| k.clone())
            .collect();
        for key in to_remove {
            if let Some(entry) = sessions.remove(&key) {
                entry.cancel.cancel();
                entry.handle.abort();
            }
        }
    }

    /// Tear down all sessions for a user.
    pub async fn teardown_by_user(&self, user_id: &UserId) {
        let mut sessions = self.sessions.write().await;
        let to_remove: Vec<String> = sessions
            .iter()
            .filter(|(_, e)| &e.user_id == user_id)
            .map(|(k, _)| k.clone())
            .collect();
        for key in to_remove {
            if let Some(entry) = sessions.remove(&key) {
                entry.cancel.cancel();
                entry.handle.abort();
            }
        }
    }

    /// List session IDs for a user.
    pub async fn list_sessions(&self, user_id: &UserId) -> Vec<String> {
        let sessions = self.sessions.read().await;
        sessions
            .iter()
            .filter(|(_, e)| &e.user_id == user_id)
            .map(|(k, _)| k.clone())
            .collect()
    }

    /// Get the device_id for a session.
    pub async fn get_device_id(&self, session_id: &str) -> Option<DeviceId> {
        let sessions = self.sessions.read().await;
        sessions.get(session_id).map(|e| e.device_id.clone())
    }

    /// Get the user_id for a session.
    pub async fn get_user_id(&self, session_id: &str) -> Option<UserId> {
        let sessions = self.sessions.read().await;
        sessions.get(session_id).map(|e| e.user_id.clone())
    }

    /// Count active sessions.
    pub async fn count(&self) -> usize {
        self.sessions.read().await.len()
    }

    /// Check if a user can create another session (under the per-user limit).
    pub async fn can_create_for_user(&self, user_id: &UserId) -> bool {
        let sessions = self.sessions.read().await;
        let user_count = sessions.values().filter(|e| &e.user_id == user_id).count();
        user_count < MAX_SESSIONS_PER_USER
    }

    /// Check if a camera can accept another viewer (under the per-camera limit).
    pub async fn can_create_for_device(&self, device_id: &DeviceId) -> bool {
        let sessions = self.sessions.read().await;
        let device_count = sessions.values().filter(|e| &e.device_id == device_id).count();
        device_count < MAX_VIEWERS_PER_CAMERA
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
    async fn register_and_list() {
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
        let sessions = mgr.list_sessions(&uid).await;
        assert_eq!(sessions.len(), 1);
        assert_eq!(sessions[0], "s1");
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
        assert!(mgr.list_sessions(&uid).await.is_empty());
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
        let remaining = mgr.list_sessions(&uid).await;
        assert_eq!(remaining.len(), 1);
        assert_eq!(remaining[0], "sb-0");
    }

    #[tokio::test]
    async fn teardown_nonexistent() {
        let mgr = SessionManager::new();
        assert!(!mgr.teardown("nonexistent").await);
    }
}
