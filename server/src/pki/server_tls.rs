use anyhow::{Context, Result};
use rcgen::{CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, KeyPair};

/// Server TLS certificate and key material.
pub struct ServerTlsCert {
    pub cert_pem: String,
    pub key_pem: String,
    pub cert_der: Vec<u8>,
    pub key_der: Vec<u8>,
    pub fingerprint: String,
}

/// Load from existing PEM files.
pub fn load_server_tls(cert_pem: &str, key_pem: &str) -> Result<ServerTlsCert> {
    let cert_der =
        ghostcam::pki::pem_to_der(cert_pem).context("failed to parse server cert PEM")?;
    let key_pair = KeyPair::from_pem(key_pem).context("failed to parse server key PEM")?;
    let key_der = key_pair.serialize_der();
    let fingerprint = ghostcam::pki::sha256_hex(&cert_der);

    Ok(ServerTlsCert {
        cert_pem: cert_pem.to_string(),
        key_pem: key_pem.to_string(),
        cert_der,
        key_der,
        fingerprint,
    })
}

/// Generate a new self-signed server TLS certificate.
/// CN="ghostcam-server", serverAuth key usage, 20-year validity.
pub fn generate_server_tls() -> Result<ServerTlsCert> {
    let key_pair = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
        .context("failed to generate server TLS key pair")?;

    let san = vec!["ghostcam-server".to_string()];
    let mut params =
        CertificateParams::new(san).context("failed to create server TLS cert params")?;
    params
        .distinguished_name
        .push(DnType::CommonName, "ghostcam-server");
    params.is_ca = IsCa::NoCa;
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    params.not_before = std::time::SystemTime::now().into();
    params.not_after = (std::time::SystemTime::now()
        + std::time::Duration::from_secs(20 * 365 * 24 * 3600))
    .into();

    let cert = params
        .self_signed(&key_pair)
        .context("failed to self-sign server TLS certificate")?;

    let cert_pem = cert.pem();
    let key_pem = key_pair.serialize_pem();
    let cert_der = cert.der().to_vec();
    let key_der = key_pair.serialize_der();
    let fingerprint = ghostcam::pki::sha256_hex(&cert_der);

    Ok(ServerTlsCert {
        cert_pem,
        key_pem,
        cert_der,
        key_der,
        fingerprint,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generate_server_tls_cert() {
        let tls = generate_server_tls().unwrap();
        assert!(tls.cert_pem.contains("BEGIN CERTIFICATE"));
        assert!(tls.key_pem.contains("BEGIN PRIVATE KEY"));
        assert!(!tls.cert_der.is_empty());
        assert!(!tls.key_der.is_empty());

        let cn = ghostcam::pki::extract_cn(&tls.cert_der).unwrap();
        assert_eq!(cn, "ghostcam-server");
    }

    #[test]
    fn load_roundtrip() {
        let tls1 = generate_server_tls().unwrap();
        let tls2 = load_server_tls(&tls1.cert_pem, &tls1.key_pem).unwrap();
        assert_eq!(tls1.fingerprint, tls2.fingerprint);
        assert_eq!(tls1.cert_der, tls2.cert_der);
    }

    #[test]
    fn fingerprint_deterministic() {
        let tls = generate_server_tls().unwrap();
        let fp1 = ghostcam::pki::sha256_hex(&tls.cert_der);
        let fp2 = ghostcam::pki::sha256_hex(&tls.cert_der);
        assert_eq!(fp1, fp2);
        assert_eq!(fp1, tls.fingerprint);
    }
}
