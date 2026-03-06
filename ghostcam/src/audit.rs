use ring::hmac;
use serde::Serialize;
use std::sync::atomic::{AtomicU64, Ordering};
use tokio::sync::broadcast;
use tracing::info;

#[derive(Debug, Clone, Serialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum AuditEvent {
    AuthSuccess {
        remote_addr: String,
        path: String,
    },
    AuthFailure {
        remote_addr: String,
        path: String,
    },
    CameraConnect {
        device_id: String,
        group_id: String,
        remote_addr: String,
    },
    CameraDisconnect {
        device_id: String,
        reason: String,
    },
    ViewerSessionCreate {
        session_id: String,
        group_id: String,
        camera_count: usize,
    },
    ViewerSessionDelete {
        session_id: String,
    },
    GroupChange {
        device_id: String,
        old_group: String,
        new_group: String,
    },
}

#[derive(Debug, Clone, Serialize)]
pub struct AuditEntry {
    pub timestamp: u64,
    pub seq: u64,
    pub event: AuditEvent,
    pub hmac: String,
}

pub struct AuditLogger {
    hmac_key: hmac::Key,
    seq: AtomicU64,
    tx: broadcast::Sender<AuditEntry>,
}

impl AuditLogger {
    pub fn new(key_bytes: &[u8]) -> Self {
        let hmac_key = hmac::Key::new(hmac::HMAC_SHA256, key_bytes);
        let (tx, _) = broadcast::channel(1024);
        Self {
            hmac_key,
            seq: AtomicU64::new(0),
            tx,
        }
    }

    pub fn log(&self, event: AuditEvent) {
        let seq = self.seq.fetch_add(1, Ordering::Relaxed);
        let timestamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64;

        let event_json = serde_json::to_string(&event).unwrap_or_default();
        let msg = format!("{seq}:{timestamp}:{event_json}");
        let tag = hmac::sign(&self.hmac_key, msg.as_bytes());
        let hmac_hex = hex_encode(tag.as_ref());

        let entry = AuditEntry {
            timestamp,
            seq,
            event: event.clone(),
            hmac: hmac_hex,
        };

        info!(
            target: "audit",
            seq = seq,
            timestamp = timestamp,
            event = %event_json,
            "audit"
        );

        let _ = self.tx.send(entry);
    }

    pub fn subscribe(&self) -> broadcast::Receiver<AuditEntry> {
        self.tx.subscribe()
    }
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn audit_event_serializes_tagged() {
        let event = AuditEvent::CameraConnect {
            device_id: "cam-01".into(),
            group_id: "default".into(),
            remote_addr: "127.0.0.1:5000".into(),
        };
        let json = serde_json::to_string(&event).unwrap();
        assert!(json.contains("\"event\":\"camera_connect\""));
        assert!(json.contains("\"device_id\":\"cam-01\""));
    }

    #[test]
    fn audit_logger_increments_seq() {
        let logger = AuditLogger::new(b"test-key");
        logger.log(AuditEvent::AuthSuccess {
            remote_addr: "1.2.3.4".into(),
            path: "/api/v1/groups".into(),
        });
        logger.log(AuditEvent::AuthSuccess {
            remote_addr: "1.2.3.4".into(),
            path: "/api/v1/groups".into(),
        });
        assert_eq!(logger.seq.load(Ordering::Relaxed), 2);
    }

    #[test]
    fn audit_entry_has_valid_hmac() {
        let logger = AuditLogger::new(b"test-key");
        let mut rx = logger.subscribe();
        logger.log(AuditEvent::CameraDisconnect {
            device_id: "cam-01".into(),
            reason: "timeout".into(),
        });
        let entry = rx.try_recv().unwrap();
        assert_eq!(entry.seq, 0);
        assert!(!entry.hmac.is_empty());
        assert_eq!(entry.hmac.len(), 64); // SHA-256 = 32 bytes = 64 hex chars
    }
}
