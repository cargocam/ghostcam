use ring::hmac;
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tracing::error;

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

/// Audit logger that writes HMAC-chained JSON lines via an async channel.
pub struct AuditLogger {
    tx: mpsc::UnboundedSender<AuditEvent>,
}

impl AuditLogger {
    /// Start an audit logger that writes to the given file path.
    /// Returns the logger handle. The background writer task runs until the sender is dropped.
    pub fn start(hmac_key: &str, log_path: std::path::PathBuf) -> Self {
        let key = hmac::Key::new(hmac::HMAC_SHA256, hmac_key.as_bytes());
        let (tx, mut rx) = mpsc::unbounded_channel::<AuditEvent>();

        tokio::spawn(async move {
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

                let entry = AuditEntry {
                    timestamp,
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

                prev_hmac = hmac_hex;
            }
        });

        Self { tx }
    }

    /// Log an audit event (non-blocking).
    pub fn log(&self, event: AuditEvent) {
        let _ = self.tx.send(event);
    }
}

/// Verify the HMAC chain in an audit log file.
/// Returns Ok(count) on success or Err with the line number that failed.
pub fn verify_audit_log(hmac_key: &str, contents: &str) -> Result<usize, (usize, String)> {
    let key = hmac::Key::new(hmac::HMAC_SHA256, hmac_key.as_bytes());
    let mut prev_hmac = "0".repeat(64);
    let mut count = 0;

    for (i, line) in contents.lines().enumerate() {
        if line.trim().is_empty() {
            continue;
        }

        let entry: AuditEntry = serde_json::from_str(line)
            .map_err(|e| (i + 1, format!("parse error: {e}")))?;

        let event_json = serde_json::to_string(&entry.event)
            .map_err(|e| (i + 1, format!("re-serialize error: {e}")))?;

        let expected = compute_hmac(&key, &event_json, &prev_hmac);

        if entry.hmac != expected {
            return Err((
                i + 1,
                format!("HMAC mismatch: expected {expected}, got {}", entry.hmac),
            ));
        }

        prev_hmac = entry.hmac;
        count += 1;
    }

    Ok(count)
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
            AuditEvent::AuthSuccess { user_id: "u1".into(), ip: "1.2.3.4".into() },
            AuditEvent::AuthFailure { username: "bad".into(), ip: "1.2.3.4".into() },
            AuditEvent::CameraConnected { device_id: "c1".into(), ip: "10.0.0.1".into(), firmware_version: "0.1.0".into() },
            AuditEvent::CameraDisconnected { device_id: "c1".into(), reason: "timeout".into() },
            AuditEvent::EnrollmentStarted { device_id: "c1".into(), owner_id: "o1".into() },
            AuditEvent::EnrollmentCompleted { device_id: "c1".into(), owner_id: "o1".into() },
            AuditEvent::EnrollmentRejected { device_id: "c1".into(), reason: "bad token".into() },
            AuditEvent::CameraRenamed { device_id: "c1".into(), old_name: "old".into(), new_name: "new".into() },
            AuditEvent::CameraRebooted { device_id: "c1".into(), initiated_by: "admin".into() },
            AuditEvent::CameraUnregistered { device_id: "c1".into(), initiated_by: "admin".into() },
            AuditEvent::CameraCommandSent { device_id: "c1".into(), command_type: "reboot".into() },
            AuditEvent::CameraGroupChanged { device_id: "c1".into(), old_group: "a".into(), new_group: "b".into() },
            AuditEvent::SessionCreated { session_id: "s1".into(), device_id: "c1".into(), viewer_ip: "1.2.3.4".into() },
            AuditEvent::SessionDestroyed { session_id: "s1".into() },
            AuditEvent::ServerStarted { version: "0.1.0".into() },
            AuditEvent::ServerStopped {},
        ];

        for event in events {
            let json = serde_json::to_string(&event).unwrap();
            let _: AuditEvent = serde_json::from_str(&json).unwrap();
        }
    }

    #[test]
    fn hmac_chain_valid() {
        let key_str = "test-hmac-key";
        let key = hmac::Key::new(hmac::HMAC_SHA256, key_str.as_bytes());
        let mut prev_hmac = "0".repeat(64);

        let mut lines = Vec::new();
        for i in 0..3 {
            let event = AuditEvent::CameraConnected {
                device_id: format!("cam-{i:02}"),
                ip: "10.0.0.1".into(),
                firmware_version: "0.1.0".into(),
            };
            let event_json = serde_json::to_string(&event).unwrap();
            let hmac_hex = compute_hmac(&key, &event_json, &prev_hmac);
            let entry = AuditEntry {
                timestamp: "2026-01-01T00:00:00Z".into(),
                event,
                hmac: hmac_hex.clone(),
            };
            lines.push(serde_json::to_string(&entry).unwrap());
            prev_hmac = hmac_hex;
        }

        let contents = lines.join("\n");
        let result = verify_audit_log(key_str, &contents);
        assert_eq!(result, Ok(3));
    }

    #[test]
    fn hmac_chain_tampered() {
        let key_str = "test-hmac-key";
        let key = hmac::Key::new(hmac::HMAC_SHA256, key_str.as_bytes());
        let mut prev_hmac = "0".repeat(64);

        let mut lines = Vec::new();
        for i in 0..3 {
            let event = AuditEvent::AuthSuccess {
                user_id: format!("user-{i}"),
                ip: "1.2.3.4".into(),
            };
            let event_json = serde_json::to_string(&event).unwrap();
            let hmac_hex = compute_hmac(&key, &event_json, &prev_hmac);
            let entry = AuditEntry {
                timestamp: "2026-01-01T00:00:00Z".into(),
                event,
                hmac: hmac_hex.clone(),
            };
            lines.push(serde_json::to_string(&entry).unwrap());
            prev_hmac = hmac_hex;
        }

        // Tamper with the second entry
        let mut tampered: AuditEntry = serde_json::from_str(&lines[1]).unwrap();
        tampered.event = AuditEvent::AuthSuccess {
            user_id: "TAMPERED".into(),
            ip: "1.2.3.4".into(),
        };
        lines[1] = serde_json::to_string(&tampered).unwrap();

        let contents = lines.join("\n");
        let result = verify_audit_log(key_str, &contents);
        assert!(result.is_err());
        let (line, _msg) = result.unwrap_err();
        assert_eq!(line, 2);
    }

    #[test]
    fn first_entry_uses_zero_hmac() {
        let key_str = "test-key";
        let key = hmac::Key::new(hmac::HMAC_SHA256, key_str.as_bytes());
        let zero_hmac = "0".repeat(64);

        let event = AuditEvent::ServerStarted { version: "0.1.0".into() };
        let event_json = serde_json::to_string(&event).unwrap();
        let hmac_hex = compute_hmac(&key, &event_json, &zero_hmac);

        let entry = AuditEntry {
            timestamp: "2026-01-01T00:00:00Z".into(),
            event,
            hmac: hmac_hex,
        };
        let line = serde_json::to_string(&entry).unwrap();

        let result = verify_audit_log(key_str, &line);
        assert_eq!(result, Ok(1));
    }
}
