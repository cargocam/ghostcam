use std::collections::HashMap;

use ghostcam::types::UserId;
use serde::Serialize;
use tokio::sync::{broadcast, RwLock};

/// Events pushed to observers via Server-Sent Events.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum SseEvent {
    CameraOnline { device_id: String },
    CameraOffline { device_id: String },
}

const SSE_CHANNEL_CAPACITY: usize = 64;

/// Per-user broadcast channels for SSE fan-out.
pub struct SseEventBus {
    channels: RwLock<HashMap<UserId, broadcast::Sender<SseEvent>>>,
}

impl SseEventBus {
    pub fn new() -> Self {
        Self {
            channels: RwLock::new(HashMap::new()),
        }
    }

    /// Subscribe to events for a user. Creates channel on first subscribe.
    pub async fn subscribe(&self, user_id: &UserId) -> broadcast::Receiver<SseEvent> {
        let mut channels = self.channels.write().await;
        let tx = channels
            .entry(user_id.clone())
            .or_insert_with(|| broadcast::channel(SSE_CHANNEL_CAPACITY).0);
        tx.subscribe()
    }

    /// Publish an event to all subscribers for a user.
    pub async fn publish(&self, user_id: &UserId, event: SseEvent) {
        let channels = self.channels.read().await;
        if let Some(tx) = channels.get(user_id) {
            let _ = tx.send(event);
        }
    }
}

impl Default for SseEventBus {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn subscribe_and_receive() {
        let bus = SseEventBus::new();
        let uid = UserId::from("user-1");
        let mut rx = bus.subscribe(&uid).await;

        bus.publish(
            &uid,
            SseEvent::CameraOnline {
                device_id: "cam-1".into(),
            },
        )
        .await;

        let event = rx.recv().await.unwrap();
        assert!(matches!(event, SseEvent::CameraOnline { .. }));
    }

    #[tokio::test]
    async fn multiple_subscribers() {
        let bus = SseEventBus::new();
        let uid = UserId::from("user-1");
        let mut rx1 = bus.subscribe(&uid).await;
        let mut rx2 = bus.subscribe(&uid).await;
        let mut rx3 = bus.subscribe(&uid).await;

        bus.publish(
            &uid,
            SseEvent::CameraOnline {
                device_id: "cam-1".into(),
            },
        )
        .await;

        assert!(rx1.recv().await.is_ok());
        assert!(rx2.recv().await.is_ok());
        assert!(rx3.recv().await.is_ok());
    }

    #[tokio::test]
    async fn events_scoped_to_user() {
        let bus = SseEventBus::new();
        let user_a = UserId::from("user-a");
        let user_b = UserId::from("user-b");
        let mut rx_a = bus.subscribe(&user_a).await;
        let _rx_b = bus.subscribe(&user_b).await;

        bus.publish(
            &user_b,
            SseEvent::CameraOnline {
                device_id: "cam-1".into(),
            },
        )
        .await;

        // User A should not receive user B's event
        assert!(rx_a.try_recv().is_err());
    }

    #[tokio::test]
    async fn subscribe_no_events() {
        let bus = SseEventBus::new();
        let uid = UserId::from("user-1");
        let mut rx = bus.subscribe(&uid).await;

        // No publish — try_recv should fail
        assert!(rx.try_recv().is_err());
    }
}
