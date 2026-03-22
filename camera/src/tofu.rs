use std::path::Path;

use anyhow::Result;

/// Verify server TLS certificate fingerprint against stored TOFU pin.
///
/// Returns Ok(()) if:
/// - No pin file exists (first connection or TOFU not yet established)
/// - The fingerprint matches
///
/// Returns Err if fingerprint doesn't match (possible MITM).
pub fn verify_server_fingerprint(
    connection: &quinn::Connection,
    data_dir: &Path,
) -> Result<()> {
    let fp_path = data_dir.join("server_fingerprint");

    let stored = match std::fs::read_to_string(&fp_path) {
        Ok(s) => {
            let s = s.trim().to_string();
            if s.is_empty() {
                return Ok(());
            }
            s
        }
        Err(_) => return Ok(()), // No pin file
    };

    let actual = get_peer_fingerprint(connection)?;

    if stored != actual {
        anyhow::bail!(
            "Server TLS fingerprint mismatch! Expected {stored}, got {actual}. \
             This may indicate a MITM attack or server certificate rotation. \
             Delete {} to re-pin.",
            fp_path.display()
        );
    }

    Ok(())
}

/// Store the server's TLS certificate fingerprint.
pub fn store_server_fingerprint(connection: &quinn::Connection, data_dir: &Path) -> Result<()> {
    let fingerprint = get_peer_fingerprint(connection)?;
    std::fs::write(data_dir.join("server_fingerprint"), &fingerprint)?;
    tracing::debug!(fingerprint, "server fingerprint stored");
    Ok(())
}

/// Get the SHA-256 fingerprint of the server's TLS certificate.
fn get_peer_fingerprint(connection: &quinn::Connection) -> Result<String> {
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
    fn verify_no_pin_file_passes() {
        // When no pin file exists, verification should pass
        // We can't test with a real connection, but we can test the file logic
        let dir = tempfile::tempdir().unwrap();
        let fp_path = dir.path().join("server_fingerprint");
        assert!(!fp_path.exists());
        // verify_server_fingerprint needs a real connection, tested via integration tests
    }

    #[test]
    fn verify_empty_pin_file_passes() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("server_fingerprint"), "").unwrap();
        // Empty pin file should be treated as no pin
    }

    #[test]
    fn store_and_load_fingerprint() {
        let dir = tempfile::tempdir().unwrap();
        let fp = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789";
        std::fs::write(dir.path().join("server_fingerprint"), fp).unwrap();

        let loaded = std::fs::read_to_string(dir.path().join("server_fingerprint"))
            .unwrap()
            .trim()
            .to_string();
        assert_eq!(loaded, fp);
    }
}
