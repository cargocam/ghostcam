use std::path::Path;

use anyhow::{Context, Result};
use ghostcam::wire::alert::Alert;
use ghostcam::wire::command::Command;
use ghostcam::wire::framing;
use ghostcam::wire::frames::InboundStreamTag;

/// Data extracted from an enrollment JWT.
pub struct EnrollmentData {
    pub server_addr: String,
    pub token: String,
}

/// Result of a successful enrollment.
pub struct EnrollmentResult {
    pub user_cert_pem: String,
    pub ca_pem: Option<String>,
    pub server_fingerprint: String,
}

/// Parse an enrollment JWT without signature verification.
/// The server will verify the signature during enrollment.
pub fn parse_enrollment_jwt(jwt: &str) -> Result<EnrollmentData> {
    let mut validation = jsonwebtoken::Validation::default();
    validation.insecure_disable_signature_validation();
    validation.validate_exp = false;
    validation.required_spec_claims.clear();

    let token_data = jsonwebtoken::decode::<EnrollmentClaims>(
        jwt,
        &jsonwebtoken::DecodingKey::from_secret(&[]),
        &validation,
    )
    .context("failed to decode enrollment JWT")?;

    let claims = token_data.claims;
    if claims.server_addr.is_empty() {
        anyhow::bail!("enrollment JWT missing server_addr claim");
    }

    Ok(EnrollmentData {
        server_addr: claims.server_addr,
        token: jwt.to_string(),
    })
}

#[derive(serde::Deserialize)]
struct EnrollmentClaims {
    #[serde(default)]
    server_addr: String,
    #[allow(dead_code)]
    #[serde(flatten)]
    extra: std::collections::HashMap<String, serde_json::Value>,
}

/// Run the enrollment flow: connect to server, send JWT + CSR, receive signed cert.
pub async fn enroll(
    enrollment: &EnrollmentData,
    device_cert: &[u8],
    device_key: &[u8],
) -> Result<EnrollmentResult> {
    // Build QUIC client with device cert only (no user cert).
    // Enrollment always skips TOFU — the server fingerprint is captured from
    // this connection and stored as the initial pin.
    let endpoint = crate::quic::build_client_endpoint(
        device_cert,
        device_key,
        None,
        true, // no_tofu: enrollment is the initial trust establishment
        Path::new("/var/ghostcam"), // not used when no_tofu=true
    )?;
    let connection = crate::quic::connect(&endpoint, &enrollment.server_addr).await?;

    tracing::info!("connected to server for enrollment");

    // Get server fingerprint (TOFU)
    let server_fingerprint = crate::tofu::get_peer_fingerprint(&connection)?;

    // Open bidirectional control stream
    let (mut send, mut recv) = connection.open_bi().await?;

    // Write Alerts stream tag
    send.write_all(&[InboundStreamTag::Alerts as u8]).await?;

    // Send enrollment alert with JWT
    let enrollment_alert = Alert::Enrollment {
        token: enrollment.token.clone(),
    };
    framing::write_json(&mut send, &enrollment_alert)
        .await
        .map_err(|e| anyhow::anyhow!("enrollment write error: {e}"))?;

    // Generate and send CSR
    let device_key_pem = std::fs::read_to_string(
        Path::new(&std::env::var("GHOSTCAM_DATA_DIR").unwrap_or_else(|_| "/var/ghostcam".into()))
            .join("device.key"),
    )
    .unwrap_or_default();

    let csr_pem = if !device_key_pem.is_empty() {
        let kp = rcgen::KeyPair::from_pem(&device_key_pem)
            .context("parsing device key for CSR")?;
        ghostcam::pki::create_csr("ghostcam-device", &kp)?
    } else {
        // Fallback: generate a fresh CSR from the DER key via PEM conversion
        let key_pem = pem::encode(&pem::Pem::new("PRIVATE KEY", device_key.to_vec()));
        let kp = rcgen::KeyPair::from_pem(&key_pem)
            .context("parsing device key DER for CSR")?;
        ghostcam::pki::create_csr("ghostcam-device", &kp)?
    };

    let csr_alert = Alert::Csr { csr_pem };
    framing::write_json(&mut send, &csr_alert)
        .await
        .map_err(|e| anyhow::anyhow!("CSR write error: {e}"))?;

    tracing::info!("enrollment request sent, waiting for response");

    // Read the server's response (CertRefresh or rejection)
    let response: Command = framing::read_json(&mut recv)
        .await
        .map_err(|e| anyhow::anyhow!("enrollment response read error: {e}"))?
        .ok_or_else(|| anyhow::anyhow!("enrollment stream closed without response"))?;

    match response {
        Command::CertRefresh {
            cert_pem, ca_pem, ..
        } => {
            tracing::info!("enrollment successful — received signed certificate");

            // Send ack so the server finalizes enrollment
            let ack = Alert::Ack {
                cmd: "cert_refresh".to_string(),
                seq: 0,
            };
            framing::write_json(&mut send, &ack)
                .await
                .map_err(|e| anyhow::anyhow!("ack write error: {e}"))?;
            send.finish()
                .map_err(|e| anyhow::anyhow!("stream finish error: {e}"))?;
            // Wait briefly for the ack to be delivered
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;

            Ok(EnrollmentResult {
                user_cert_pem: cert_pem,
                ca_pem,
                server_fingerprint,
            })
        }
        Command::Unregister { .. } => {
            anyhow::bail!("enrollment rejected by server");
        }
        other => {
            anyhow::bail!("unexpected response during enrollment: {other:?}");
        }
    }
}

/// Store enrollment result to disk.
pub async fn store_enrollment(
    data_dir: &Path,
    result: &EnrollmentResult,
    server_addr: &str,
) -> Result<()> {
    // Write user certificate PEM
    tokio::fs::write(data_dir.join("user.crt"), &result.user_cert_pem)
        .await
        .context("writing user certificate")?;

    // Copy device key as user key (the user cert was signed from the device's CSR)
    let device_key_path = data_dir.join("device.key");
    if device_key_path.exists() {
        tokio::fs::copy(&device_key_path, data_dir.join("user.key"))
            .await
            .context("copying device key as user key")?;
    }

    // If CA cert provided, write it too
    if let Some(ca_pem) = &result.ca_pem {
        tokio::fs::write(data_dir.join("ca.crt"), ca_pem)
            .await
            .context("writing CA certificate")?;
    }

    // Write server fingerprint (TOFU)
    tokio::fs::write(
        data_dir.join("server_fingerprint"),
        &result.server_fingerprint,
    )
    .await
    .context("writing server fingerprint")?;

    // Store server address for reconnection
    tokio::fs::write(data_dir.join("server.addr"), server_addr)
        .await
        .context("writing server address")?;

    tracing::info!("enrollment data stored");
    Ok(())
}

/// Clear enrollment state (for unregistration).
pub async fn clear_enrollment(data_dir: &Path) -> Result<()> {
    let _ = tokio::fs::remove_file(data_dir.join("user.crt")).await;
    let _ = tokio::fs::remove_file(data_dir.join("user.key")).await;
    let _ = tokio::fs::remove_file(data_dir.join("server_fingerprint")).await;
    // Keep device.key and device.crt — device identity persists
    tracing::info!("enrollment state cleared");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Create a test JWT with HS256 (signature won't be verified).
    fn make_test_jwt(claims: &serde_json::Value) -> String {
        use base64::Engine;
        let header = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(r#"{"alg":"HS256","typ":"JWT"}"#);
        let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(serde_json::to_string(claims).unwrap());
        // Fake signature — parse_enrollment_jwt disables verification
        let sig = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(b"fakesig");
        format!("{header}.{payload}.{sig}")
    }

    #[test]
    fn parse_valid_enrollment_jwt() {
        let jwt = make_test_jwt(&serde_json::json!({
            "server_addr": "10.0.0.1:4433",
            "sub": "user-1"
        }));

        let data = parse_enrollment_jwt(&jwt).unwrap();
        assert_eq!(data.server_addr, "10.0.0.1:4433");
        assert_eq!(data.token, jwt);
    }

    #[test]
    fn parse_jwt_missing_server_addr() {
        let jwt = make_test_jwt(&serde_json::json!({
            "sub": "user-1"
        }));

        let result = parse_enrollment_jwt(&jwt);
        assert!(result.is_err());
    }

    #[test]
    fn parse_invalid_jwt_string() {
        let result = parse_enrollment_jwt("not-a-jwt");
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn clear_enrollment_removes_files() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("user.crt"), "cert").unwrap();
        std::fs::write(dir.path().join("server_fingerprint"), "fp").unwrap();

        clear_enrollment(dir.path()).await.unwrap();

        assert!(!dir.path().join("user.crt").exists());
        assert!(!dir.path().join("server_fingerprint").exists());
    }

    #[tokio::test]
    async fn store_enrollment_writes_files() {
        let dir = tempfile::tempdir().unwrap();
        let result = EnrollmentResult {
            user_cert_pem: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----".into(),
            ca_pem: Some("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----".into()),
            server_fingerprint: "abcdef1234567890".into(),
        };

        store_enrollment(dir.path(), &result, "10.0.0.1:4433")
            .await
            .unwrap();

        assert!(dir.path().join("user.crt").exists());
        assert!(dir.path().join("ca.crt").exists());
        assert!(dir.path().join("server_fingerprint").exists());
        assert!(dir.path().join("server.addr").exists());

        let addr = std::fs::read_to_string(dir.path().join("server.addr")).unwrap();
        assert_eq!(addr, "10.0.0.1:4433");
    }
}
