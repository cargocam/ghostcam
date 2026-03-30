use crate::types::CertFingerprint;
use rcgen::{CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, KeyPair, KeyUsagePurpose};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum PkiError {
    #[error("certificate generation failed: {0}")]
    CertGen(#[from] rcgen::Error),

    #[error("invalid PEM: {0}")]
    InvalidPem(String),
}

/// Generate a new P-256 key pair. Returns (KeyPair, private key DER bytes).
pub fn generate_key_pair() -> Result<(KeyPair, Vec<u8>), PkiError> {
    let kp = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)?;
    let der = kp.serialize_der();
    Ok((kp, der))
}

/// Create a self-signed CA certificate. Returns (cert DER, key pair).
pub fn create_self_signed_ca(
    cn: &str,
    validity_years: u32,
) -> Result<(Vec<u8>, KeyPair), PkiError> {
    let (kp, _) = generate_key_pair()?;
    let mut params = CertificateParams::new(vec![])?;
    params.distinguished_name.push(DnType::CommonName, cn);
    params.is_ca = IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    params.not_before = std::time::SystemTime::now().into();
    params.not_after = (std::time::SystemTime::now()
        + std::time::Duration::from_secs(365 * 24 * 3600 * validity_years as u64))
    .into();
    let cert = params.self_signed(&kp)?;
    Ok((cert.der().to_vec(), kp))
}

/// Create a self-signed server TLS certificate. Returns (cert DER, key pair).
pub fn create_self_signed_server_cert(
    cn: &str,
    validity_years: u32,
) -> Result<(Vec<u8>, KeyPair), PkiError> {
    let (kp, _) = generate_key_pair()?;
    let san = vec![cn.to_string()];
    let mut params = CertificateParams::new(san)?;
    params.distinguished_name.push(DnType::CommonName, cn);
    params.is_ca = IsCa::NoCa;
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    params.not_before = std::time::SystemTime::now().into();
    params.not_after = (std::time::SystemTime::now()
        + std::time::Duration::from_secs(365 * 24 * 3600 * validity_years as u64))
    .into();
    let cert = params.self_signed(&kp)?;
    Ok((cert.der().to_vec(), kp))
}

/// Create a self-signed device identity certificate. Returns (cert DER, key pair).
pub fn create_device_cert() -> Result<(Vec<u8>, KeyPair), PkiError> {
    let (kp, _) = generate_key_pair()?;
    let mut params = CertificateParams::new(vec![])?;
    params
        .distinguished_name
        .push(DnType::CommonName, "ghostcam-device");
    params.is_ca = IsCa::NoCa;
    params.not_before = std::time::SystemTime::now().into();
    params.not_after = (std::time::SystemTime::now()
        + std::time::Duration::from_secs(20 * 365 * 24 * 3600))
    .into();
    let cert = params.self_signed(&kp)?;
    Ok((cert.der().to_vec(), kp))
}

/// Compute SHA-256 fingerprint of DER-encoded certificate bytes.
pub fn cert_fingerprint(cert_der: &[u8]) -> CertFingerprint {
    CertFingerprint(sha256_hex(cert_der))
}

/// Compute SHA-256 hex digest of arbitrary bytes.
pub fn sha256_hex(data: &[u8]) -> String {
    let digest = ring::digest::digest(&ring::digest::SHA256, data);
    digest.as_ref().iter().map(|b| format!("{b:02x}")).collect()
}

/// Extract the Common Name (CN) from a DER-encoded certificate.
pub fn extract_cn(cert_der: &[u8]) -> Result<String, PkiError> {
    use x509_parser::prelude::*;
    let (_, cert) = X509Certificate::from_der(cert_der)
        .map_err(|e| PkiError::InvalidPem(format!("failed to parse DER certificate: {e}")))?;
    for attr in cert.subject().iter_common_name() {
        if let Ok(cn) = attr.as_str() {
            return Ok(cn.to_string());
        }
    }
    Err(PkiError::InvalidPem("no CN in certificate subject".into()))
}

/// Extract the serial number from a DER-encoded certificate as a hex string.
pub fn cert_serial_hex(cert_der: &[u8]) -> Result<String, PkiError> {
    use x509_parser::prelude::*;
    let (_, cert) = X509Certificate::from_der(cert_der)
        .map_err(|e| PkiError::InvalidPem(format!("failed to parse DER certificate: {e}")))?;
    let serial = cert.serial.to_str_radix(16);
    Ok(serial)
}

/// Convert PEM-encoded certificate to DER bytes.
pub fn pem_to_der(pem_str: &str) -> Result<Vec<u8>, PkiError> {
    let parsed =
        pem::parse(pem_str).map_err(|e| PkiError::InvalidPem(format!("invalid PEM: {e}")))?;
    Ok(parsed.into_contents())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generate_key_pair_produces_valid_p256() {
        let (kp, der) = generate_key_pair().unwrap();
        assert!(!der.is_empty());
        let _ = kp.serialize_der();
    }

    #[test]
    fn create_ca_cert_works() {
        let (cert, _kp) = create_self_signed_ca("Test CA", 20).unwrap();
        assert!(!cert.is_empty());
    }

    #[test]
    fn create_server_cert_works() {
        let (cert, _kp) = create_self_signed_server_cert("localhost", 20).unwrap();
        assert!(!cert.is_empty());
    }

    #[test]
    fn create_device_cert_works() {
        let (cert, _kp) = create_device_cert().unwrap();
        assert!(!cert.is_empty());
    }

    #[test]
    fn cert_fingerprint_deterministic() {
        let (cert, _) = create_device_cert().unwrap();
        let fp1 = cert_fingerprint(&cert);
        let fp2 = cert_fingerprint(&cert);
        assert_eq!(fp1, fp2);
    }

    #[test]
    fn cert_fingerprint_different_certs() {
        let (cert1, _) = create_device_cert().unwrap();
        let (cert2, _) = create_device_cert().unwrap();
        let fp1 = cert_fingerprint(&cert1);
        let fp2 = cert_fingerprint(&cert2);
        assert_ne!(fp1, fp2);
    }

    #[test]
    fn sha256_hex_deterministic() {
        let data = b"test data";
        let h1 = sha256_hex(data);
        let h2 = sha256_hex(data);
        assert_eq!(h1, h2);
        assert_eq!(h1.len(), 64);
    }

    #[test]
    fn extract_cn_from_cert() {
        let (cert, _) = create_self_signed_server_cert("test-device", 1).unwrap();
        let cn = extract_cn(&cert).unwrap();
        assert_eq!(cn, "test-device");
    }

    #[test]
    fn extract_cn_from_ca_cert() {
        let (cert, _) = create_self_signed_ca("My CA", 1).unwrap();
        let cn = extract_cn(&cert).unwrap();
        assert_eq!(cn, "My CA");
    }

    #[test]
    fn serial_number_from_der() {
        let (cert, _) = create_device_cert().unwrap();
        let serial = cert_serial_hex(&cert).unwrap();
        assert!(!serial.is_empty());
    }

    #[test]
    fn serial_number_different_certs() {
        let (cert1, _) = create_device_cert().unwrap();
        let (cert2, _) = create_device_cert().unwrap();
        let s1 = cert_serial_hex(&cert1).unwrap();
        let s2 = cert_serial_hex(&cert2).unwrap();
        assert_ne!(s1, s2);
    }

    #[test]
    fn pem_to_der_roundtrip() {
        let (ca_cert_der, _) = create_self_signed_ca("Test CA", 1).unwrap();
        let pem_encoded = pem::Pem::new("CERTIFICATE", ca_cert_der.clone());
        let cert_pem = pem::encode(&pem_encoded);
        let cert_der = pem_to_der(&cert_pem).unwrap();
        assert_eq!(cert_der, ca_cert_der);
        let cn = extract_cn(&cert_der).unwrap();
        assert_eq!(cn, "Test CA");
    }
}
