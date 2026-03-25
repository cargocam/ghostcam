use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use anyhow::Result;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, Error as TlsError, SignatureScheme};

/// Trust-On-First-Use server certificate verifier.
///
/// On first connection (no stored fingerprint): accepts any server cert and stores
/// its SHA-256 fingerprint to disk.
///
/// On subsequent connections: verifies the server cert fingerprint matches the
/// stored one. Rejects on mismatch (possible MITM).
#[derive(Debug)]
pub struct TofuServerCertVerifier {
    data_dir: PathBuf,
    /// Cached fingerprint (None = not yet loaded from disk)
    cached_fingerprint: Arc<Mutex<Option<String>>>,
}

impl TofuServerCertVerifier {
    /// Create a new TOFU verifier. Loads any existing fingerprint from disk.
    pub fn new(data_dir: &Path) -> Self {
        let fp_path = data_dir.join("server_fingerprint");
        let cached = match std::fs::read_to_string(&fp_path) {
            Ok(s) => {
                let s = s.trim().to_string();
                if s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()) {
                    tracing::info!("loaded stored server fingerprint for TOFU verification");
                    Some(s)
                } else if s.is_empty() {
                    None
                } else {
                    tracing::warn!(
                        "stored server fingerprint has unexpected format — treating as unpinned"
                    );
                    None
                }
            }
            Err(_) => None,
        };

        Self {
            data_dir: data_dir.to_path_buf(),
            cached_fingerprint: Arc::new(Mutex::new(cached)),
        }
    }
}

impl ServerCertVerifier for TofuServerCertVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, TlsError> {
        let actual = ghostcam::pki::sha256_hex(end_entity.as_ref());

        let mut cached = self
            .cached_fingerprint
            .lock()
            .unwrap_or_else(|e| e.into_inner());

        match cached.as_deref() {
            Some(stored) if stored == actual => {
                tracing::debug!("server fingerprint matches stored TOFU pin");
                Ok(ServerCertVerified::assertion())
            }
            Some(stored) => {
                let fp_path = self.data_dir.join("server_fingerprint");
                tracing::error!(
                    expected = stored,
                    actual = actual,
                    pin_file = %fp_path.display(),
                    "TOFU fingerprint mismatch — possible MITM attack! \
                     Delete the pin file to re-pin."
                );
                Err(TlsError::General(format!(
                    "server TLS fingerprint mismatch: expected {stored}, got {actual}. \
                     Delete {} to re-pin.",
                    fp_path.display()
                )))
            }
            None => {
                // First connection — TOFU: accept and store
                let fp_path = self.data_dir.join("server_fingerprint");
                if let Err(e) = std::fs::write(&fp_path, &actual) {
                    tracing::error!(
                        path = %fp_path.display(),
                        error = %e,
                        "failed to write server fingerprint to disk — rejecting connection"
                    );
                    return Err(TlsError::General(format!(
                        "TOFU: cannot persist server fingerprint to {}: {e}",
                        fp_path.display()
                    )));
                }

                tracing::info!(fingerprint = %actual, "TOFU: pinning server fingerprint on first connect");
                *cached = Some(actual.clone());
                Ok(ServerCertVerified::assertion())
            }
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &rustls::crypto::ring::default_provider().signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &rustls::crypto::ring::default_provider().signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        rustls::crypto::ring::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}

/// Get the SHA-256 fingerprint of the server's TLS certificate.
pub fn get_peer_fingerprint(connection: &quinn::Connection) -> Result<String> {
    let peer_certs = connection
        .peer_identity()
        .and_then(|id| {
            id.downcast::<Vec<rustls::pki_types::CertificateDer<'static>>>()
                .ok()
        })
        .ok_or_else(|| anyhow::anyhow!("no peer certificate"))?;

    let leaf = peer_certs
        .first()
        .ok_or_else(|| anyhow::anyhow!("empty peer cert chain"))?;

    Ok(ghostcam::pki::sha256_hex(leaf.as_ref()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tofu_initial_state_no_file_is_unpinned() {
        let dir = tempfile::tempdir().unwrap();
        let verifier = TofuServerCertVerifier::new(dir.path());

        // No fingerprint stored yet
        assert!(verifier.cached_fingerprint.lock().unwrap().is_none());
        assert!(!dir.path().join("server_fingerprint").exists());
    }

    #[test]
    fn tofu_loads_existing_fingerprint() {
        let dir = tempfile::tempdir().unwrap();
        let fp = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789";
        std::fs::write(dir.path().join("server_fingerprint"), fp).unwrap();

        let verifier = TofuServerCertVerifier::new(dir.path());
        assert_eq!(
            verifier.cached_fingerprint.lock().unwrap().as_deref(),
            Some(fp)
        );
    }

    #[test]
    fn tofu_empty_pin_file_treated_as_no_pin() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("server_fingerprint"), "").unwrap();

        let verifier = TofuServerCertVerifier::new(dir.path());
        assert!(verifier.cached_fingerprint.lock().unwrap().is_none());
    }

    #[test]
    fn tofu_verify_first_connect_accepts_and_stores() {
        let dir = tempfile::tempdir().unwrap();
        let verifier = TofuServerCertVerifier::new(dir.path());

        let fake_cert = CertificateDer::from(vec![1, 2, 3, 4, 5]);
        let expected_fp = ghostcam::pki::sha256_hex(&[1, 2, 3, 4, 5]);
        let server_name = ServerName::try_from("ghostcam").unwrap();

        let result =
            verifier.verify_server_cert(&fake_cert, &[], &server_name, &[], UnixTime::now());
        assert!(result.is_ok());

        // Fingerprint should be cached
        assert_eq!(
            verifier.cached_fingerprint.lock().unwrap().as_deref(),
            Some(expected_fp.as_str())
        );

        // Fingerprint should be written to disk
        let stored = std::fs::read_to_string(dir.path().join("server_fingerprint")).unwrap();
        assert_eq!(stored, expected_fp);
    }

    #[test]
    fn tofu_verify_matching_fingerprint_succeeds() {
        let dir = tempfile::tempdir().unwrap();
        let fake_cert = CertificateDer::from(vec![1, 2, 3, 4, 5]);
        let expected_fp = ghostcam::pki::sha256_hex(&[1, 2, 3, 4, 5]);
        std::fs::write(dir.path().join("server_fingerprint"), &expected_fp).unwrap();

        let verifier = TofuServerCertVerifier::new(dir.path());
        let server_name = ServerName::try_from("ghostcam").unwrap();

        let result =
            verifier.verify_server_cert(&fake_cert, &[], &server_name, &[], UnixTime::now());
        assert!(result.is_ok());
    }

    #[test]
    fn tofu_verify_mismatched_fingerprint_fails() {
        let dir = tempfile::tempdir().unwrap();
        let wrong_fp = "0000000000000000000000000000000000000000000000000000000000000000";
        std::fs::write(dir.path().join("server_fingerprint"), wrong_fp).unwrap();

        let verifier = TofuServerCertVerifier::new(dir.path());
        let fake_cert = CertificateDer::from(vec![1, 2, 3, 4, 5]);
        let server_name = ServerName::try_from("ghostcam").unwrap();

        let result =
            verifier.verify_server_cert(&fake_cert, &[], &server_name, &[], UnixTime::now());
        assert!(result.is_err());

        let err = result.unwrap_err();
        let msg = format!("{err}");
        assert!(
            msg.contains("fingerprint mismatch"),
            "error should mention mismatch: {msg}"
        );
    }

    #[test]
    fn tofu_invalid_format_treated_as_unpinned() {
        let dir = tempfile::tempdir().unwrap();

        // Too short
        std::fs::write(dir.path().join("server_fingerprint"), "abcdef").unwrap();
        let verifier = TofuServerCertVerifier::new(dir.path());
        assert!(verifier.cached_fingerprint.lock().unwrap().is_none());

        // Non-hex characters
        std::fs::write(
            dir.path().join("server_fingerprint"),
            "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
        )
        .unwrap();
        let verifier = TofuServerCertVerifier::new(dir.path());
        assert!(verifier.cached_fingerprint.lock().unwrap().is_none());
    }

    #[test]
    fn tofu_first_connect_rejects_on_disk_write_failure() {
        // Use a non-existent directory so write fails
        let dir = Path::new("/nonexistent/tofu/test");
        let verifier = TofuServerCertVerifier::new(dir);

        let fake_cert = CertificateDer::from(vec![1, 2, 3, 4, 5]);
        let server_name = ServerName::try_from("ghostcam").unwrap();

        let result =
            verifier.verify_server_cert(&fake_cert, &[], &server_name, &[], UnixTime::now());
        assert!(
            result.is_err(),
            "should reject when fingerprint can't be persisted"
        );

        let msg = format!("{}", result.unwrap_err());
        assert!(
            msg.contains("cannot persist"),
            "error should mention persist failure: {msg}"
        );
    }
}
