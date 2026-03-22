use anyhow::{Context, Result};
use jsonwebtoken::{Algorithm, DecodingKey, EncodingKey, Header, Validation};
use rcgen::{
    CertificateParams, CertificateSigningRequestParams, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose,
};
use rustls::pki_types::CertificateDer;

use super::enrollment::EnrollmentClaims;

/// Holds the CA certificate and key in memory for the lifetime of the server process.
pub struct CaManager {
    ca_cert_pem: String,
    ca_cert_der: Vec<u8>,
    ca_key_pair: KeyPair,
    ca_cert: rcgen::Certificate,
    jwt_encoding_key: EncodingKey,
    jwt_decoding_key: DecodingKey,
}

fn build_jwt_keys(key_pair: &KeyPair) -> Result<(EncodingKey, DecodingKey)> {
    let key_pem = key_pair.serialize_pem();
    let encoding_key = EncodingKey::from_ec_pem(key_pem.as_bytes())
        .context("failed to create JWT encoding key")?;

    // For ES256 decoding, we need the public key in PEM format.
    // rcgen's public_key_der() returns the raw SubjectPublicKeyInfo (SPKI) DER.
    // Wrap it in PEM so jsonwebtoken can parse it.
    let spki_der = key_pair.public_key_der();
    let public_pem = pem::Pem::new("PUBLIC KEY", spki_der);
    let public_pem_str = pem::encode(&public_pem);
    let decoding_key = DecodingKey::from_ec_pem(public_pem_str.as_bytes())
        .context("failed to create JWT decoding key")?;

    Ok((encoding_key, decoding_key))
}

impl CaManager {
    /// Load from existing PEM files.
    pub fn from_pem(cert_pem: &str, key_pem: &str) -> Result<Self> {
        let ca_key_pair =
            KeyPair::from_pem(key_pem).context("failed to parse CA private key PEM")?;

        let ca_cert_der =
            ghostcam::pki::pem_to_der(cert_pem).context("failed to parse CA certificate PEM")?;

        let ca_cert_der_typed = CertificateDer::from(ca_cert_der.clone());
        let ca_cert_params = CertificateParams::from_ca_cert_der(&ca_cert_der_typed)
            .context("failed to parse CA cert DER into params")?;
        let ca_cert = ca_cert_params
            .self_signed(&ca_key_pair)
            .context("failed to reconstruct CA certificate")?;

        let (jwt_encoding_key, jwt_decoding_key) = build_jwt_keys(&ca_key_pair)?;

        Ok(Self {
            ca_cert_pem: cert_pem.to_string(),
            ca_cert_der,
            ca_key_pair,
            ca_cert,
            jwt_encoding_key,
            jwt_decoding_key,
        })
    }

    /// Generate a new self-signed Instance CA.
    /// Returns (CaManager, cert_pem, key_pem).
    pub fn generate_instance_ca() -> Result<(Self, String, String)> {
        let ca_key_pair = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
            .context("failed to generate CA key pair")?;

        let mut params =
            CertificateParams::new(vec![]).context("failed to create CA cert params")?;
        params
            .distinguished_name
            .push(DnType::CommonName, "Ghostcam Instance CA");
        params.is_ca = IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.not_before = std::time::SystemTime::now().into();
        params.not_after = (std::time::SystemTime::now()
            + std::time::Duration::from_secs(20 * 365 * 24 * 3600))
        .into();

        let ca_cert = params
            .self_signed(&ca_key_pair)
            .context("failed to self-sign CA certificate")?;

        let cert_pem = ca_cert.pem();
        let key_pem = ca_key_pair.serialize_pem();
        let ca_cert_der = ca_cert.der().to_vec();

        let (jwt_encoding_key, jwt_decoding_key) = build_jwt_keys(&ca_key_pair)?;

        let manager = Self {
            ca_cert_pem: cert_pem.clone(),
            ca_cert_der,
            ca_key_pair,
            ca_cert,
            jwt_encoding_key,
            jwt_decoding_key,
        };

        Ok((manager, cert_pem, key_pem))
    }

    /// Get the CA certificate PEM.
    pub fn ca_cert_pem(&self) -> &str {
        &self.ca_cert_pem
    }

    /// Get the CA certificate DER.
    pub fn ca_cert_der(&self) -> &[u8] {
        &self.ca_cert_der
    }

    /// Sign a CSR, issuing a user association certificate.
    /// Subject CN={device_id}. clientAuth key usage. Far-future expiry.
    pub fn sign_csr(&self, csr_pem: &str, device_id: &str) -> Result<String> {
        let csr = CertificateSigningRequestParams::from_pem(csr_pem)
            .context("failed to parse CSR PEM")?;

        let mut params = csr.params;
        params
            .distinguished_name
            .push(DnType::CommonName, device_id);
        params.is_ca = IsCa::NoCa;
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        params.not_before = std::time::SystemTime::now().into();
        // ~100 years: effectively no expiry
        params.not_after = (std::time::SystemTime::now()
            + std::time::Duration::from_secs(100 * 365 * 24 * 3600))
        .into();

        let signed = params
            .signed_by(&csr.public_key, &self.ca_cert, &self.ca_key_pair)
            .context("failed to sign CSR")?;

        Ok(signed.pem())
    }

    /// Sign an enrollment JWT (ES256).
    pub fn sign_enrollment_jwt(&self, claims: &EnrollmentClaims) -> Result<String> {
        let header = Header::new(Algorithm::ES256);
        let token = jsonwebtoken::encode(&header, claims, &self.jwt_encoding_key)
            .context("failed to sign enrollment JWT")?;
        Ok(token)
    }

    /// Verify and decode an enrollment JWT.
    pub fn verify_enrollment_jwt(&self, token: &str) -> Result<EnrollmentClaims> {
        let mut validation = Validation::new(Algorithm::ES256);
        validation.set_issuer(&["ghostcam-server"]);
        validation.set_required_spec_claims(&["exp", "iss", "jti"]);

        let data =
            jsonwebtoken::decode::<EnrollmentClaims>(token, &self.jwt_decoding_key, &validation)
                .context("failed to verify enrollment JWT")?;

        Ok(data.claims)
    }

    /// Verify a user association certificate was signed by this CA.
    pub fn verify_user_cert(&self, cert_der: &[u8]) -> Result<()> {
        use x509_parser::prelude::*;

        let (_, cert) = X509Certificate::from_der(cert_der)
            .map_err(|e| anyhow::anyhow!("failed to parse user cert DER: {e}"))?;

        // Parse the CA cert to get its SubjectPublicKeyInfo
        let (_, ca_cert) = X509Certificate::from_der(&self.ca_cert_der)
            .map_err(|e| anyhow::anyhow!("failed to parse CA cert DER: {e}"))?;

        // Verify the issuer CN matches our CA CN
        let ca_cn =
            ghostcam::pki::extract_cn(&self.ca_cert_der).context("failed to extract CA CN")?;

        let mut issuer_cn = None;
        for attr in cert.issuer().iter_common_name() {
            if let Ok(cn) = attr.as_str() {
                issuer_cn = Some(cn.to_string());
                break;
            }
        }

        let issuer_cn = issuer_cn.ok_or_else(|| anyhow::anyhow!("no CN in user cert issuer"))?;

        if issuer_cn != ca_cn {
            anyhow::bail!(
                "user cert issuer CN '{}' does not match CA CN '{}'",
                issuer_cn,
                ca_cn
            );
        }

        // Verify the signature using the CA's SubjectPublicKeyInfo
        cert.verify_signature(Some(ca_cert.public_key()))
            .map_err(|e| anyhow::anyhow!("user cert signature verification failed: {e}"))?;

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generate_instance_ca() {
        let (ca, cert_pem, key_pem) = CaManager::generate_instance_ca().unwrap();
        assert!(cert_pem.contains("BEGIN CERTIFICATE"));
        assert!(key_pem.contains("BEGIN PRIVATE KEY"));

        let cn = ghostcam::pki::extract_cn(ca.ca_cert_der()).unwrap();
        assert_eq!(cn, "Ghostcam Instance CA");
    }

    #[test]
    fn from_pem_roundtrip() {
        let (ca1, cert_pem, key_pem) = CaManager::generate_instance_ca().unwrap();
        let ca2 = CaManager::from_pem(&cert_pem, &key_pem).unwrap();

        // Both should be able to sign a CSR and produce valid certs
        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();

        let signed1 = ca1.sign_csr(&csr, "dev-01").unwrap();
        let signed2 = ca2.sign_csr(&csr, "dev-01").unwrap();

        // Both produce valid certs (CN matches)
        let der1 = ghostcam::pki::pem_to_der(&signed1).unwrap();
        let der2 = ghostcam::pki::pem_to_der(&signed2).unwrap();
        assert_eq!(ghostcam::pki::extract_cn(&der1).unwrap(), "dev-01");
        assert_eq!(ghostcam::pki::extract_cn(&der2).unwrap(), "dev-01");
    }

    #[test]
    fn sign_csr_subject_cn() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();
        let signed = ca.sign_csr(&csr, "cam-42").unwrap();
        let der = ghostcam::pki::pem_to_der(&signed).unwrap();
        assert_eq!(ghostcam::pki::extract_cn(&der).unwrap(), "cam-42");
    }

    #[test]
    fn sign_csr_issuer() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();
        let signed = ca.sign_csr(&csr, "cam-01").unwrap();
        let der = ghostcam::pki::pem_to_der(&signed).unwrap();

        use x509_parser::prelude::*;
        let (_, cert) = X509Certificate::from_der(&der).unwrap();
        let mut issuer_cn = None;
        for attr in cert.issuer().iter_common_name() {
            if let Ok(cn) = attr.as_str() {
                issuer_cn = Some(cn.to_string());
            }
        }
        assert_eq!(issuer_cn.unwrap(), "Ghostcam Instance CA");
    }

    #[test]
    fn sign_csr_client_auth() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();
        let signed = ca.sign_csr(&csr, "cam-01").unwrap();
        let der = ghostcam::pki::pem_to_der(&signed).unwrap();

        use x509_parser::prelude::*;
        let (_, cert) = X509Certificate::from_der(&der).unwrap();
        let eku = cert
            .extended_key_usage()
            .expect("should have EKU extension")
            .expect("should parse EKU");
        assert!(eku.value.client_auth);
    }

    #[test]
    fn sign_csr_invalid_pem() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let result = ca.sign_csr("not a csr", "cam-01");
        assert!(result.is_err());
    }

    #[test]
    fn verify_user_cert_valid() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();
        let signed = ca.sign_csr(&csr, "cam-01").unwrap();
        let der = ghostcam::pki::pem_to_der(&signed).unwrap();

        ca.verify_user_cert(&der).unwrap();
    }

    #[test]
    fn verify_user_cert_wrong_ca() {
        let (ca_a, _, _) = CaManager::generate_instance_ca().unwrap();
        let (ca_b, _, _) = CaManager::generate_instance_ca().unwrap();

        let (device_kp, _) = ghostcam::pki::generate_key_pair().unwrap();
        let csr = ghostcam::pki::create_csr("test", &device_kp).unwrap();
        let signed = ca_a.sign_csr(&csr, "cam-01").unwrap();
        let der = ghostcam::pki::pem_to_der(&signed).unwrap();

        let result = ca_b.verify_user_cert(&der);
        assert!(result.is_err());
    }

    #[test]
    fn sign_enrollment_jwt() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        let token = ca.sign_enrollment_jwt(&claims).unwrap();
        assert!(token.starts_with("ey"));
    }

    #[test]
    fn verify_enrollment_jwt_roundtrip() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let claims = EnrollmentClaims::new("127.0.0.1:4433", Some("Kitchen".into()), None);
        let token = ca.sign_enrollment_jwt(&claims).unwrap();
        let decoded = ca.verify_enrollment_jwt(&token).unwrap();
        assert_eq!(decoded.server_addr, "127.0.0.1:4433");
        assert_eq!(decoded.display_name.as_deref(), Some("Kitchen"));
        assert_eq!(decoded.iss, "ghostcam-server");
    }

    #[test]
    fn verify_enrollment_jwt_expired() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let mut claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        claims.exp = 1; // Unix epoch + 1s — long expired
        let token = ca.sign_enrollment_jwt(&claims).unwrap();
        let result = ca.verify_enrollment_jwt(&token);
        assert!(result.is_err());
    }

    #[test]
    fn verify_enrollment_jwt_wrong_key() {
        let (ca_a, _, _) = CaManager::generate_instance_ca().unwrap();
        let (ca_b, _, _) = CaManager::generate_instance_ca().unwrap();
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        let token = ca_a.sign_enrollment_jwt(&claims).unwrap();
        let result = ca_b.verify_enrollment_jwt(&token);
        assert!(result.is_err());
    }
}
