use serde::{Deserialize, Serialize};

/// Camera → Server alert messages, sent on the alert unidirectional QUIC stream.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Alert {
    Handshake {
        protocol_version: u32,
        fw_version: String,
        streams: Vec<StreamKind>,
    },
    CapabilityUpdate {
        streams: Vec<StreamKind>,
    },
    RecordingSegment {
        device_id: String,
        segment_id: String,
        start_ts: u64,
        end_ts: u64,
        size_bytes: u64,
    },
    SegmentEvicted {
        segment_id: String,
    },
    SegmentUploaded {
        seq: u64,
        segment_id: String,
    },
    SegmentUploadFailed {
        seq: u64,
        segment_id: String,
        reason: UploadFailReason,
    },
    Ack {
        cmd: String,
        seq: u64,
    },
    Enrollment {
        token: String,
    },
    Csr {
        csr_pem: String,
    },
    StorageFull {
        free_bytes: u64,
    },
    StorageResumed {
        free_bytes: u64,
    },
    UpdateApplying {
        version: String,
    },
    UpdateSucceeded {
        version: String,
    },
    UpdateFailed {
        version_attempted: String,
        version_current: String,
        reason: UpdateFailReason,
    },
    Networks {
        networks: Vec<NetworkEntry>,
    },
}

/// Stream types the camera is capable of producing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StreamKind {
    Video,
    Audio,
    Telemetry,
}

/// Reason a segment upload failed.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum UploadFailReason {
    Evicted,
    NotFound,
    IoError,
}

/// Reason a firmware update failed.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum UpdateFailReason {
    Watchdog,
    HashMismatch,
    DownloadFailed,
}

/// A WiFi network visible to the camera.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct NetworkEntry {
    pub ssid: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub signal_dbm: Option<i8>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn alert_handshake_roundtrip() {
        let alert = Alert::Handshake {
            protocol_version: 1,
            fw_version: "0.1.0".into(),
            streams: vec![StreamKind::Video, StreamKind::Audio],
        };
        let json = serde_json::to_string(&alert).unwrap();
        let back: Alert = serde_json::from_str(&json).unwrap();
        assert_eq!(alert, back);
        assert!(json.contains(r#""type":"handshake""#));
    }

    #[test]
    fn alert_all_variants_roundtrip() {
        let alerts = vec![
            Alert::Handshake {
                protocol_version: 1,
                fw_version: "0.1.0".into(),
                streams: vec![StreamKind::Video],
            },
            Alert::CapabilityUpdate {
                streams: vec![StreamKind::Audio, StreamKind::Telemetry],
            },
            Alert::RecordingSegment {
                device_id: "cam-01".into(),
                segment_id: "seg-001".into(),
                start_ts: 1000,
                end_ts: 2000,
                size_bytes: 50000,
            },
            Alert::SegmentEvicted {
                segment_id: "seg-001".into(),
            },
            Alert::SegmentUploaded {
                seq: 1,
                segment_id: "seg-001".into(),
            },
            Alert::SegmentUploadFailed {
                seq: 2,
                segment_id: "seg-002".into(),
                reason: UploadFailReason::Evicted,
            },
            Alert::Ack {
                cmd: "start_video".into(),
                seq: 3,
            },
            Alert::Enrollment {
                token: "eyJ0eXAiOiJKV1QiLCJhbGciOiJFUzI1NiJ9.test".into(),
            },
            Alert::Csr {
                csr_pem:
                    "-----BEGIN CERTIFICATE REQUEST-----\ntest\n-----END CERTIFICATE REQUEST-----"
                        .into(),
            },
            Alert::StorageFull { free_bytes: 0 },
            Alert::StorageResumed {
                free_bytes: 1_000_000,
            },
            Alert::UpdateApplying {
                version: "1.0.0".into(),
            },
            Alert::UpdateSucceeded {
                version: "1.0.0".into(),
            },
            Alert::UpdateFailed {
                version_attempted: "1.0.0".into(),
                version_current: "0.9.0".into(),
                reason: UpdateFailReason::HashMismatch,
            },
            Alert::Networks {
                networks: vec![NetworkEntry {
                    ssid: "CameraNet".into(),
                    signal_dbm: Some(-62),
                }],
            },
        ];

        for alert in &alerts {
            let json = serde_json::to_string(alert).unwrap();
            let back: Alert = serde_json::from_str(&json).unwrap();
            assert_eq!(*alert, back, "roundtrip failed for {json}");
        }
    }

    #[test]
    fn alert_unknown_type_rejected() {
        let json = r#"{"type":"totally_unknown","foo":"bar"}"#;
        let result = serde_json::from_str::<Alert>(json);
        assert!(result.is_err());
    }

    #[test]
    fn stream_kind_serde() {
        assert_eq!(
            serde_json::to_string(&StreamKind::Video).unwrap(),
            r#""video""#
        );
        assert_eq!(
            serde_json::to_string(&StreamKind::Audio).unwrap(),
            r#""audio""#
        );
        assert_eq!(
            serde_json::to_string(&StreamKind::Telemetry).unwrap(),
            r#""telemetry""#
        );
    }

    #[test]
    fn upload_fail_reason_serde() {
        for reason in [
            UploadFailReason::Evicted,
            UploadFailReason::NotFound,
            UploadFailReason::IoError,
        ] {
            let json = serde_json::to_string(&reason).unwrap();
            let back: UploadFailReason = serde_json::from_str(&json).unwrap();
            assert_eq!(reason, back);
        }
    }

    #[test]
    fn update_fail_reason_serde() {
        for reason in [
            UpdateFailReason::Watchdog,
            UpdateFailReason::HashMismatch,
            UpdateFailReason::DownloadFailed,
        ] {
            let json = serde_json::to_string(&reason).unwrap();
            let back: UpdateFailReason = serde_json::from_str(&json).unwrap();
            assert_eq!(reason, back);
        }
    }

    #[test]
    fn network_entry_with_signal() {
        let entry = NetworkEntry {
            ssid: "Test".into(),
            signal_dbm: Some(-62),
        };
        let json = serde_json::to_string(&entry).unwrap();
        assert!(json.contains("signal_dbm"));
        let back: NetworkEntry = serde_json::from_str(&json).unwrap();
        assert_eq!(entry, back);
    }

    #[test]
    fn network_entry_without_signal() {
        let entry = NetworkEntry {
            ssid: "Test".into(),
            signal_dbm: None,
        };
        let json = serde_json::to_string(&entry).unwrap();
        assert!(!json.contains("signal_dbm"));
        let back: NetworkEntry = serde_json::from_str(&json).unwrap();
        assert_eq!(entry, back);
    }
}
