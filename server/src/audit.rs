use std::sync::Arc;

use ring::hmac;
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tracing::error;

use crate::db_trait::Database;

/// All auditable events in the system.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum AuditEvent {
    // Auth
    AuthSuccess {
        user_id: String,
        ip: String,
    },
    AuthFailure {
        username: String,
        ip: String,
    },

    // Camera lifecycle
    CameraConnected {
        device_id: String,
        ip: String,
        firmware_version: String,
    },
    CameraDisconnected {
        device_id: String,
        reason: String,
    },

    // Enrollment
    EnrollmentStarted {
        device_id: String,
        owner_id: String,
    },
    EnrollmentCompleted {
        device_id: String,
        owner_id: String,
    },
    EnrollmentRejected {
        device_id: String,
        reason: String,
    },

    // Camera management
    CameraRenamed {
        device_id: String,
        old_name: String,
        new_name: String,
    },
    CameraRebooted {
        device_id: String,
        initiated_by: String,
    },
    CameraUnregistered {
        device_id: String,
        initiated_by: String,
    },
    CameraCommandSent {
        device_id: String,
        command_type: String,
    },

    // Group/config
    CameraGroupChanged {
        device_id: String,
        old_group: String,
        new_group: String,
    },

    // Session
    SessionCreated {
        session_id: String,
        device_id: String,
        viewer_ip: String,
    },
    SessionDestroyed {
        session_id: String,
    },

    // Server
    ServerStarted {
        version: String,
    },
    ServerStopped {},
}

/// A single tamper-evident audit log entry.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuditEntry {
    pub timestamp: String,
    pub event: AuditEvent,
    pub hmac: String,
}

/// Computes an HMAC-SHA256 over the event JSON + previous HMAC (chaining).
fn compute_hmac(key: &hmac::Key, event_json: &str, prev_hmac: &str) -> String {
    let mut ctx = hmac::Context::with_key(key);
    ctx.update(prev_hmac.as_bytes());
    ctx.update(event_json.as_bytes());
    let tag = ctx.sign();
    hex::encode(tag.as_ref())
}

/// Extract the event type tag from an `AuditEvent` for database indexing.
fn event_type_tag(event: &AuditEvent) -> &'static str {
    match event {
        AuditEvent::AuthSuccess { .. } => "auth_success",
        AuditEvent::AuthFailure { .. } => "auth_failure",
        AuditEvent::CameraConnected { .. } => "camera_connected",
        AuditEvent::CameraDisconnected { .. } => "camera_disconnected",
        AuditEvent::EnrollmentStarted { .. } => "enrollment_started",
        AuditEvent::EnrollmentCompleted { .. } => "enrollment_completed",
        AuditEvent::EnrollmentRejected { .. } => "enrollment_rejected",
        AuditEvent::CameraRenamed { .. } => "camera_renamed",
        AuditEvent::CameraRebooted { .. } => "camera_rebooted",
        AuditEvent::CameraUnregistered { .. } => "camera_unregistered",
        AuditEvent::CameraCommandSent { .. } => "camera_command_sent",
        AuditEvent::CameraGroupChanged { .. } => "camera_group_changed",
        AuditEvent::SessionCreated { .. } => "session_created",
        AuditEvent::SessionDestroyed { .. } => "session_destroyed",
        AuditEvent::ServerStarted { .. } => "server_started",
        AuditEvent::ServerStopped { .. } => "server_stopped",
    }
}

/// Audit logger that writes HMAC-chained JSON lines via an async channel.
/// Optionally persists entries to the database.
pub struct AuditLogger {
    tx: tokio::sync::Mutex<Option<mpsc::Sender<AuditEvent>>>,
    /// Handle to the background writer task for clean shutdown.
    #[allow(dead_code)]
    handle: tokio::sync::Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl AuditLogger {
    /// Start an audit logger that writes to the given file path (no database persistence).
    /// Returns the logger handle. The background writer task runs until the sender is dropped.
    #[cfg(test)]
    pub fn start(hmac_key: &str, log_path: std::path::PathBuf) -> Self {
        Self::start_inner(hmac_key, log_path, None)
    }

    /// Start an audit logger that writes to the given file path and persists to the database.
    pub fn start_with_db(
        hmac_key: &str,
        log_path: std::path::PathBuf,
        db: Arc<dyn Database>,
    ) -> Self {
        Self::start_inner(hmac_key, log_path, Some(db))
    }

    fn start_inner(
        hmac_key: &str,
        log_path: std::path::PathBuf,
        db: Option<Arc<dyn Database>>,
    ) -> Self {
        let key = hmac::Key::new(hmac::HMAC_SHA256, hmac_key.as_bytes());
        let (tx, mut rx) = mpsc::channel::<AuditEvent>(4096);

        let handle = tokio::spawn(async move {
            use tokio::io::AsyncWriteExt;

            let file = match tokio::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&log_path)
                .await
            {
                Ok(f) => f,
                Err(e) => {
                    error!(path = %log_path.display(), error = %e, "Failed to open audit log");
                    return;
                }
            };

            let mut writer = tokio::io::BufWriter::new(file);
            let mut prev_hmac = "0".repeat(64); // zero HMAC for first entry

            while let Some(event) = rx.recv().await {
                let timestamp = chrono::Utc::now().to_rfc3339();
                let event_json = match serde_json::to_string(&event) {
                    Ok(j) => j,
                    Err(e) => {
                        error!(error = %e, "Failed to serialize audit event");
                        continue;
                    }
                };

                let hmac_hex = compute_hmac(&key, &event_json, &prev_hmac);
                let type_tag = event_type_tag(&event);

                let entry = AuditEntry {
                    timestamp: timestamp.clone(),
                    event,
                    hmac: hmac_hex.clone(),
                };

                match serde_json::to_string(&entry) {
                    Ok(line) => {
                        let _ = writer.write_all(line.as_bytes()).await;
                        let _ = writer.write_all(b"\n").await;
                        let _ = writer.flush().await;
                    }
                    Err(e) => {
                        error!(error = %e, "Failed to serialize audit entry");
                    }
                }

                // Persist to database (non-fatal on failure)
                if let Some(ref db) = db {
                    let event_data: serde_json::Value =
                        serde_json::from_str(&event_json).unwrap_or_default();
                    if let Err(e) = db
                        .insert_audit_entry(&timestamp, type_tag, &event_data, &hmac_hex)
                        .await
                    {
                        error!(error = %e, "Failed to persist audit entry to database");
                    }
                }

                prev_hmac = hmac_hex;
            }
        });

        Self {
            tx: tokio::sync::Mutex::new(Some(tx)),
            handle: tokio::sync::Mutex::new(Some(handle)),
        }
    }

    /// Log an audit event (non-blocking). Drops the event if the channel is full.
    pub fn log(&self, event: AuditEvent) {
        if let Ok(guard) = self.tx.try_lock() {
            if let Some(ref tx) = *guard {
                if tx.try_send(event).is_err() {
                    tracing::warn!("audit channel full — dropping event");
                }
            }
        }
    }

    /// Shut down the logger: close the channel and wait for all pending events to flush.
    #[allow(dead_code)]
    pub async fn shutdown(&self) {
        // Drop the sender to close the channel
        self.tx.lock().await.take();
        // Wait for the background task to finish processing remaining events
        if let Some(handle) = self.handle.lock().await.take() {
            let _ = handle.await;
        }
    }
}

/// Extract the client IP from request headers.
/// Checks X-Forwarded-For first, then X-Real-IP, then falls back to "unknown".
/// Extracted values are validated as IP addresses to prevent log injection.
pub fn client_ip(headers: &axum::http::HeaderMap) -> String {
    if let Some(xff) = headers.get("x-forwarded-for").and_then(|v| v.to_str().ok()) {
        if let Some(first) = xff.split(',').next() {
            if first.trim().parse::<std::net::IpAddr>().is_ok() {
                return first.trim().to_string();
            }
        }
    }
    if let Some(xri) = headers.get("x-real-ip").and_then(|v| v.to_str().ok()) {
        if xri.trim().parse::<std::net::IpAddr>().is_ok() {
            return xri.trim().to_string();
        }
    }
    "unknown".to_string()
}

// hex encoding without an external crate
mod hex {
    pub fn encode(bytes: &[u8]) -> String {
        bytes
            .iter()
            .fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
                use std::fmt::Write;
                let _ = write!(s, "{b:02x}");
                s
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn audit_event_roundtrip() {
        let event = AuditEvent::CameraConnected {
            device_id: "cam-01".into(),
            ip: "10.0.0.1".into(),
            firmware_version: "0.1.0".into(),
        };
        let json = serde_json::to_string(&event).unwrap();
        let parsed: AuditEvent = serde_json::from_str(&json).unwrap();
        assert!(matches!(parsed, AuditEvent::CameraConnected { .. }));
    }

    #[test]
    fn audit_event_all_variants_serialize() {
        let events = vec![
            AuditEvent::AuthSuccess {
                user_id: "u1".into(),
                ip: "1.2.3.4".into(),
            },
            AuditEvent::AuthFailure {
                username: "bad".into(),
                ip: "1.2.3.4".into(),
            },
            AuditEvent::CameraConnected {
                device_id: "c1".into(),
                ip: "10.0.0.1".into(),
                firmware_version: "0.1.0".into(),
            },
            AuditEvent::CameraDisconnected {
                device_id: "c1".into(),
                reason: "timeout".into(),
            },
            AuditEvent::EnrollmentStarted {
                device_id: "c1".into(),
                owner_id: "o1".into(),
            },
            AuditEvent::EnrollmentCompleted {
                device_id: "c1".into(),
                owner_id: "o1".into(),
            },
            AuditEvent::EnrollmentRejected {
                device_id: "c1".into(),
                reason: "bad token".into(),
            },
            AuditEvent::CameraRenamed {
                device_id: "c1".into(),
                old_name: "old".into(),
                new_name: "new".into(),
            },
            AuditEvent::CameraRebooted {
                device_id: "c1".into(),
                initiated_by: "admin".into(),
            },
            AuditEvent::CameraUnregistered {
                device_id: "c1".into(),
                initiated_by: "admin".into(),
            },
            AuditEvent::CameraCommandSent {
                device_id: "c1".into(),
                command_type: "reboot".into(),
            },
            AuditEvent::CameraGroupChanged {
                device_id: "c1".into(),
                old_group: "a".into(),
                new_group: "b".into(),
            },
            AuditEvent::SessionCreated {
                session_id: "s1".into(),
                device_id: "c1".into(),
                viewer_ip: "1.2.3.4".into(),
            },
            AuditEvent::SessionDestroyed {
                session_id: "s1".into(),
            },
            AuditEvent::ServerStarted {
                version: "0.1.0".into(),
            },
            AuditEvent::ServerStopped {},
        ];

        for event in events {
            let json = serde_json::to_string(&event).unwrap();
            let _: AuditEvent = serde_json::from_str(&json).unwrap();
        }
    }

    #[test]
    fn hmac_chain_produces_deterministic_output() {
        let key_str = "test-hmac-key";
        let key = hmac::Key::new(hmac::HMAC_SHA256, key_str.as_bytes());
        let prev_hmac = "0".repeat(64);

        let event = AuditEvent::ServerStarted {
            version: "0.1.0".into(),
        };
        let event_json = serde_json::to_string(&event).unwrap();
        let hmac1 = compute_hmac(&key, &event_json, &prev_hmac);
        let hmac2 = compute_hmac(&key, &event_json, &prev_hmac);
        assert_eq!(hmac1, hmac2);
        assert_eq!(hmac1.len(), 64); // SHA-256 = 32 bytes = 64 hex chars
    }

    #[test]
    fn hmac_chain_changes_with_different_prev() {
        let key = hmac::Key::new(hmac::HMAC_SHA256, b"test-key");
        let event_json = r#"{"type":"server_started","version":"0.1.0"}"#;
        let hmac1 = compute_hmac(&key, event_json, &"0".repeat(64));
        let hmac2 = compute_hmac(&key, event_json, &"1".repeat(64));
        assert_ne!(
            hmac1, hmac2,
            "chained HMAC should differ with different prev"
        );
    }

    #[test]
    fn event_type_tag_covers_all_variants() {
        // Verify the tag function returns distinct non-empty strings
        let events = vec![
            AuditEvent::AuthSuccess {
                user_id: "u".into(),
                ip: "i".into(),
            },
            AuditEvent::AuthFailure {
                username: "u".into(),
                ip: "i".into(),
            },
            AuditEvent::CameraConnected {
                device_id: "d".into(),
                ip: "i".into(),
                firmware_version: "f".into(),
            },
            AuditEvent::CameraDisconnected {
                device_id: "d".into(),
                reason: "r".into(),
            },
            AuditEvent::EnrollmentStarted {
                device_id: "d".into(),
                owner_id: "o".into(),
            },
            AuditEvent::EnrollmentCompleted {
                device_id: "d".into(),
                owner_id: "o".into(),
            },
            AuditEvent::EnrollmentRejected {
                device_id: "d".into(),
                reason: "r".into(),
            },
            AuditEvent::CameraRenamed {
                device_id: "d".into(),
                old_name: "o".into(),
                new_name: "n".into(),
            },
            AuditEvent::CameraRebooted {
                device_id: "d".into(),
                initiated_by: "a".into(),
            },
            AuditEvent::CameraUnregistered {
                device_id: "d".into(),
                initiated_by: "a".into(),
            },
            AuditEvent::CameraCommandSent {
                device_id: "d".into(),
                command_type: "c".into(),
            },
            AuditEvent::CameraGroupChanged {
                device_id: "d".into(),
                old_group: "a".into(),
                new_group: "b".into(),
            },
            AuditEvent::SessionCreated {
                session_id: "s".into(),
                device_id: "d".into(),
                viewer_ip: "i".into(),
            },
            AuditEvent::SessionDestroyed {
                session_id: "s".into(),
            },
            AuditEvent::ServerStarted {
                version: "v".into(),
            },
            AuditEvent::ServerStopped {},
        ];
        let mut tags: Vec<&str> = events.iter().map(event_type_tag).collect();
        let orig_len = tags.len();
        tags.sort();
        tags.dedup();
        assert_eq!(tags.len(), orig_len, "all event type tags should be unique");
    }

    #[tokio::test]
    async fn audit_logger_writes_to_file() {
        let dir = tempfile::tempdir().unwrap();
        let log_path = dir.path().join("audit.jsonl");

        let logger = AuditLogger::start("test-hmac-key", log_path.clone());
        logger.log(AuditEvent::ServerStarted {
            version: "0.1.0".into(),
        });
        logger.log(AuditEvent::AuthSuccess {
            user_id: "u1".into(),
            ip: "1.2.3.4".into(),
        });

        // Shut down cleanly: close channel and await flush
        logger.shutdown().await;

        let contents = tokio::fs::read_to_string(&log_path).await.unwrap();
        let lines: Vec<&str> = contents.lines().collect();
        assert_eq!(lines.len(), 2, "expected 2 audit entries");

        // Parse both entries
        let entry1: AuditEntry = serde_json::from_str(lines[0]).unwrap();
        let entry2: AuditEntry = serde_json::from_str(lines[1]).unwrap();

        assert!(matches!(entry1.event, AuditEvent::ServerStarted { .. }));
        assert!(matches!(entry2.event, AuditEvent::AuthSuccess { .. }));
        assert_eq!(entry1.hmac.len(), 64);
        assert_eq!(entry2.hmac.len(), 64);
        // Chain integrity: HMACs should differ
        assert_ne!(entry1.hmac, entry2.hmac);
    }

    #[tokio::test]
    async fn audit_logger_hmac_chain_verifiable() {
        let dir = tempfile::tempdir().unwrap();
        let log_path = dir.path().join("audit.jsonl");
        let hmac_key = "verify-test-key";

        let logger = AuditLogger::start(hmac_key, log_path.clone());
        logger.log(AuditEvent::ServerStarted {
            version: "1.0".into(),
        });
        logger.log(AuditEvent::CameraConnected {
            device_id: "cam-01".into(),
            ip: "10.0.0.1".into(),
            firmware_version: "0.1".into(),
        });
        logger.log(AuditEvent::ServerStopped {});

        logger.shutdown().await;

        let contents = tokio::fs::read_to_string(&log_path).await.unwrap();
        let key = hmac::Key::new(hmac::HMAC_SHA256, hmac_key.as_bytes());
        let mut prev_hmac = "0".repeat(64);

        for line in contents.lines() {
            let entry: AuditEntry = serde_json::from_str(line).unwrap();
            let event_json = serde_json::to_string(&entry.event).unwrap();
            let expected_hmac = compute_hmac(&key, &event_json, &prev_hmac);
            assert_eq!(entry.hmac, expected_hmac, "HMAC chain verification failed");
            prev_hmac = entry.hmac;
        }
    }

    #[test]
    fn client_ip_extracts_x_forwarded_for() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("x-forwarded-for", "1.2.3.4, 5.6.7.8".parse().unwrap());
        assert_eq!(client_ip(&headers), "1.2.3.4");
    }

    #[test]
    fn client_ip_extracts_x_real_ip() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("x-real-ip", "9.8.7.6".parse().unwrap());
        assert_eq!(client_ip(&headers), "9.8.7.6");
    }

    #[test]
    fn client_ip_falls_back_to_unknown() {
        let headers = axum::http::HeaderMap::new();
        assert_eq!(client_ip(&headers), "unknown");
    }

    #[test]
    fn client_ip_rejects_invalid_xff() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("x-forwarded-for", "not-an-ip, 1.2.3.4".parse().unwrap());
        // Invalid first entry is rejected, falls back to unknown
        assert_eq!(client_ip(&headers), "unknown");
    }

    #[test]
    fn client_ip_rejects_invalid_xri() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("x-real-ip", "not-a-valid-ip".parse().unwrap());
        assert_eq!(client_ip(&headers), "unknown");
    }
}
