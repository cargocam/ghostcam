use std::path::Path;

use anyhow::{Context, Result};

use super::ca::CaManager;
use super::server_tls::{self, ServerTlsCert};

/// Result of PKI bootstrap.
pub struct BootstrapResult {
    pub ca: CaManager,
    pub server_tls: ServerTlsCert,
}

/// Bootstrap the server PKI.
///
/// On first run: generates Instance CA + server TLS cert, writes PEM files.
/// On subsequent runs: loads existing PEM files.
pub async fn bootstrap_pki(data_dir: &Path) -> Result<BootstrapResult> {
    let ca_cert_path = data_dir.join("ca.crt");
    let ca_key_path = data_dir.join("ca.key");
    let server_cert_path = data_dir.join("server.crt");
    let server_key_path = data_dir.join("server.key");

    if !ca_cert_path.exists() {
        // Ensure data directory exists
        tokio::fs::create_dir_all(data_dir)
            .await
            .context("failed to create data directory")?;

        // Generate Instance CA
        let (ca, ca_cert_pem, ca_key_pem) =
            CaManager::generate_instance_ca().context("failed to generate Instance CA")?;

        tokio::fs::write(&ca_cert_path, &ca_cert_pem)
            .await
            .context("failed to write ca.crt")?;
        tokio::fs::write(&ca_key_path, &ca_key_pem)
            .await
            .context("failed to write ca.key")?;

        // Generate server TLS cert
        let server_tls =
            server_tls::generate_server_tls().context("failed to generate server TLS cert")?;

        tokio::fs::write(&server_cert_path, &server_tls.cert_pem)
            .await
            .context("failed to write server.crt")?;
        tokio::fs::write(&server_key_path, &server_tls.key_pem)
            .await
            .context("failed to write server.key")?;

        tracing::warn!(
            "First-run PKI bootstrap complete. Back up these files:\n  {}\n  {}\n  {}\n  {}",
            ca_cert_path.display(),
            ca_key_path.display(),
            server_cert_path.display(),
            server_key_path.display(),
        );

        Ok(BootstrapResult {
            ca,
            server_tls,
        })
    } else {
        // Load existing PKI material
        let ca_cert_pem = tokio::fs::read_to_string(&ca_cert_path)
            .await
            .context("failed to read ca.crt")?;
        let ca_key_pem = tokio::fs::read_to_string(&ca_key_path)
            .await
            .context("failed to read ca.key")?;
        let server_cert_pem = tokio::fs::read_to_string(&server_cert_path)
            .await
            .context("failed to read server.crt")?;
        let server_key_pem = tokio::fs::read_to_string(&server_key_path)
            .await
            .context("failed to read server.key")?;

        let ca =
            CaManager::from_pem(&ca_cert_pem, &ca_key_pem).context("failed to load CA from PEM")?;
        let server_tls = server_tls::load_server_tls(&server_cert_pem, &server_key_pem)
            .context("failed to load server TLS from PEM")?;

        Ok(BootstrapResult {
            ca,
            server_tls,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn first_run_generates_files() {
        let dir = tempfile::tempdir().unwrap();
        let _result = bootstrap_pki(dir.path()).await.unwrap();

        assert!(dir.path().join("ca.crt").exists());
        assert!(dir.path().join("ca.key").exists());
        assert!(dir.path().join("server.crt").exists());
        assert!(dir.path().join("server.key").exists());
    }

    #[tokio::test]
    async fn second_run_loads_existing() {
        let dir = tempfile::tempdir().unwrap();
        let _ = bootstrap_pki(dir.path()).await.unwrap();
        let _result = bootstrap_pki(dir.path()).await.unwrap();
    }

    #[tokio::test]
    async fn second_run_same_fingerprint() {
        let dir = tempfile::tempdir().unwrap();
        let r1 = bootstrap_pki(dir.path()).await.unwrap();
        let r2 = bootstrap_pki(dir.path()).await.unwrap();

        let fp1 = ghostcam::pki::sha256_hex(r1.ca.ca_cert_der());
        let fp2 = ghostcam::pki::sha256_hex(r2.ca.ca_cert_der());
        assert_eq!(fp1, fp2);
    }

    #[tokio::test]
    async fn files_are_valid_pem() {
        let dir = tempfile::tempdir().unwrap();
        let _ = bootstrap_pki(dir.path()).await.unwrap();

        let ca_cert = tokio::fs::read_to_string(dir.path().join("ca.crt"))
            .await
            .unwrap();
        let ca_key = tokio::fs::read_to_string(dir.path().join("ca.key"))
            .await
            .unwrap();
        assert!(ca_cert.contains("BEGIN CERTIFICATE"));
        assert!(ca_key.contains("BEGIN PRIVATE KEY"));

        // Can reconstruct CaManager from the files
        CaManager::from_pem(&ca_cert, &ca_key).unwrap();
    }
}
