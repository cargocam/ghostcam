use serde::{Deserialize, Serialize};

/// Server → Camera command messages, sent on the command unidirectional QUIC stream.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Command {
    StartVideo {
        seq: u64,
    },
    StopVideo {
        seq: u64,
    },
    StartAudio {
        seq: u64,
    },
    StopAudio {
        seq: u64,
    },
    UploadSegment {
        seq: u64,
        segment_id: String,
    },
    UploadInit {
        seq: u64,
    },
    Reboot {
        seq: u64,
    },
    NetworkConfig {
        seq: u64,
        ssid: String,
        psk: String,
    },
    RemoveNetwork {
        seq: u64,
        ssid: String,
    },
    ListNetworks {
        seq: u64,
    },
    CertRefresh {
        seq: u64,
        cert_pem: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        ca_pem: Option<String>,
    },
    Unregister {
        seq: u64,
    },
}

/// Maximum length for short string fields (IDs, versions, SSIDs).
const MAX_SHORT_STRING: usize = 256;

/// Maximum length for PEM certificate fields.
const MAX_PEM_STRING: usize = 8192;

impl Command {
    /// Validate string field lengths after deserialization.
    pub fn validate(&self) -> Result<(), String> {
        match self {
            Command::UploadSegment { segment_id, .. } => {
                if segment_id.len() > MAX_SHORT_STRING {
                    return Err(format!("segment_id too long: {} bytes", segment_id.len()));
                }
                Ok(())
            }
            Command::NetworkConfig { ssid, psk, .. } => {
                if ssid.len() > MAX_SHORT_STRING {
                    return Err(format!("ssid too long: {} bytes", ssid.len()));
                }
                if psk.len() > MAX_SHORT_STRING {
                    return Err(format!("psk too long: {} bytes", psk.len()));
                }
                Ok(())
            }
            Command::RemoveNetwork { ssid, .. } => {
                if ssid.len() > MAX_SHORT_STRING {
                    return Err(format!("ssid too long: {} bytes", ssid.len()));
                }
                Ok(())
            }
            Command::CertRefresh {
                cert_pem, ca_pem, ..
            } => {
                if cert_pem.len() > MAX_PEM_STRING {
                    return Err(format!("cert_pem too long: {} bytes", cert_pem.len()));
                }
                if let Some(ca) = ca_pem {
                    if ca.len() > MAX_PEM_STRING {
                        return Err(format!("ca_pem too long: {} bytes", ca.len()));
                    }
                }
                Ok(())
            }
            _ => Ok(()),
        }
    }

    /// Extract the sequence number from any command variant.
    pub fn seq(&self) -> u64 {
        match self {
            Command::StartVideo { seq }
            | Command::StopVideo { seq }
            | Command::StartAudio { seq }
            | Command::StopAudio { seq }
            | Command::UploadSegment { seq, .. }
            | Command::UploadInit { seq }
            | Command::Reboot { seq }
            | Command::NetworkConfig { seq, .. }
            | Command::RemoveNetwork { seq, .. }
            | Command::ListNetworks { seq }
            | Command::CertRefresh { seq, .. }
            | Command::Unregister { seq } => *seq,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_all_variants_roundtrip() {
        let commands = vec![
            Command::StartVideo { seq: 1 },
            Command::StopVideo { seq: 2 },
            Command::StartAudio { seq: 3 },
            Command::StopAudio { seq: 4 },
            Command::UploadSegment {
                seq: 5,
                segment_id: "seg-001".into(),
            },
            Command::UploadInit { seq: 6 },
            Command::Reboot { seq: 7 },
            Command::NetworkConfig {
                seq: 8,
                ssid: "CameraNet".into(),
                psk: "pass123".into(),
            },
            Command::RemoveNetwork {
                seq: 9,
                ssid: "OldNet".into(),
            },
            Command::ListNetworks { seq: 10 },
            Command::CertRefresh {
                seq: 11,
                cert_pem: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----".into(),
                ca_pem: None,
            },
            Command::Unregister { seq: 12 },
        ];

        for cmd in &commands {
            let json = serde_json::to_string(cmd).unwrap();
            let back: Command = serde_json::from_str(&json).unwrap();
            assert_eq!(*cmd, back, "roundtrip failed for {json}");
        }
    }

    #[test]
    fn command_optional_fields() {
        // CertRefresh without ca_pem
        let cmd = Command::CertRefresh {
            seq: 1,
            cert_pem: "cert".into(),
            ca_pem: None,
        };
        let json = serde_json::to_string(&cmd).unwrap();
        assert!(!json.contains("ca_pem"));

        // CertRefresh with ca_pem
        let cmd = Command::CertRefresh {
            seq: 1,
            cert_pem: "cert".into(),
            ca_pem: Some("ca".into()),
        };
        let json = serde_json::to_string(&cmd).unwrap();
        assert!(json.contains("ca_pem"));
    }

    #[test]
    fn command_seq_preserved() {
        let commands = vec![
            Command::StartVideo { seq: 42 },
            Command::StopVideo { seq: 99 },
            Command::Reboot { seq: 1000 },
        ];

        for cmd in &commands {
            let json = serde_json::to_string(cmd).unwrap();
            let back: Command = serde_json::from_str(&json).unwrap();
            assert_eq!(cmd.seq(), back.seq());
        }
    }
}
