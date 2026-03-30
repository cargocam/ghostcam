use anyhow::{Context, Result};
use jsonwebtoken::{Algorithm, DecodingKey, EncodingKey, Header, Validation};
use rcgen::{CertificateParams, DnType, IsCa, KeyPair, KeyUsagePurpose};

use super::enrollment::EnrollmentClaims;

/// Holds the CA certificate and key in memory for the lifetime of the server process.
pub struct CaManager {
    #[allow(dead_code)] // read via ca_cert_der() in tests
    ca_cert_der: Vec<u8>,
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

        let (jwt_encoding_key, jwt_decoding_key) = build_jwt_keys(&ca_key_pair)?;

        Ok(Self {
            ca_cert_der,
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
            ca_cert_der,
            jwt_encoding_key,
            jwt_decoding_key,
        };

        Ok((manager, cert_pem, key_pem))
    }

    /// Get the CA certificate DER (used in tests).
    #[cfg(test)]
    pub fn ca_cert_der(&self) -> &[u8] {
        &self.ca_cert_der
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
        let (_ca1, cert_pem, key_pem) = CaManager::generate_instance_ca().unwrap();
        let ca2 = CaManager::from_pem(&cert_pem, &key_pem).unwrap();

        // Both should produce valid JWT tokens
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        let token = ca2.sign_enrollment_jwt(&claims).unwrap();
        let decoded = ca2.verify_enrollment_jwt(&token).unwrap();
        assert_eq!(decoded.server_addr, "127.0.0.1:4433");
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

    #[test]
    fn claim_token_roundtrip() {
        let (ca, _, _) = CaManager::generate_instance_ca().unwrap();
        let claims = EnrollmentClaims::new_claim("10.0.0.1:4433", "user-42", 86400);
        let token = ca.sign_enrollment_jwt(&claims).unwrap();
        let decoded = ca.verify_enrollment_jwt(&token).unwrap();
        assert_eq!(decoded.sub.as_deref(), Some("user-42"));
        assert_eq!(decoded.server_addr, "10.0.0.1:4433");
        assert!(decoded.iat > 0);
    }
}
