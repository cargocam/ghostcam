use std::path::Path;

use anyhow::{Context, Result};

/// Load the device certificate and key from disk.
/// If they don't exist, generate new ones (first boot).
pub fn load_or_create_device_cert(
    cert_path: &Path,
    key_path: &Path,
) -> Result<(Vec<u8>, Vec<u8>)> {
    if cert_path.exists() && key_path.exists() {
        let cert_pem = std::fs::read_to_string(cert_path)
            .context("reading device cert")?;
        let key_pem = std::fs::read_to_string(key_path)
            .context("reading device key")?;

        let cert_der = ghostcam::pki::pem_to_der(&cert_pem)?;
        let key_pair = rcgen::KeyPair::from_pem(&key_pem)
            .context("parsing device key PEM")?;
        let key_der = key_pair.serialize_der();

        return Ok((cert_der, key_der));
    }

    tracing::info!("generating new device certificate");
    let (cert_der, key_pair) = ghostcam::pki::create_device_cert()?;
    let key_der = key_pair.serialize_der();

    // Write PEM files
    let cert_pem = pem::encode(&pem::Pem::new("CERTIFICATE", cert_der.clone()));
    let key_pem = key_pair.serialize_pem();

    if let Some(parent) = cert_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::write(cert_path, &cert_pem).context("writing device cert")?;
    std::fs::write(key_path, &key_pem).context("writing device key")?;

    Ok((cert_der, key_der))
}

/// Load the user association certificate and key from disk.
/// Returns None if not enrolled (files don't exist).
pub fn load_user_cert(
    cert_path: &Path,
    key_path: &Path,
) -> Result<Option<(Vec<u8>, Vec<u8>)>> {
    if !cert_path.exists() || !key_path.exists() {
        return Ok(None);
    }

    let cert_pem = std::fs::read_to_string(cert_path)
        .context("reading user cert")?;
    let key_pem = std::fs::read_to_string(key_path)
        .context("reading user key")?;

    let cert_der = ghostcam::pki::pem_to_der(&cert_pem)?;
    let key_pair = rcgen::KeyPair::from_pem(&key_pem)
        .context("parsing user key PEM")?;
    let key_der = key_pair.serialize_der();

    Ok(Some((cert_der, key_der)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn create_device_cert_on_first_boot() {
        let dir = tempfile::tempdir().unwrap();
        let cert = dir.path().join("device.crt");
        let key = dir.path().join("device.key");

        let (cert_der, key_der) = load_or_create_device_cert(&cert, &key).unwrap();
        assert!(!cert_der.is_empty());
        assert!(!key_der.is_empty());
        assert!(cert.exists());
        assert!(key.exists());
    }

    #[test]
    fn load_existing_device_cert() {
        let dir = tempfile::tempdir().unwrap();
        let cert = dir.path().join("device.crt");
        let key = dir.path().join("device.key");

        let (cert1, key1) = load_or_create_device_cert(&cert, &key).unwrap();
        let (cert2, key2) = load_or_create_device_cert(&cert, &key).unwrap();
        assert_eq!(cert1, cert2);
        assert_eq!(key1, key2);
    }

    #[test]
    fn load_user_cert_not_enrolled() {
        let dir = tempfile::tempdir().unwrap();
        let cert = dir.path().join("user.crt");
        let key = dir.path().join("user.key");
        assert!(load_user_cert(&cert, &key).unwrap().is_none());
    }

}
