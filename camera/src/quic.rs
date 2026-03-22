use std::sync::Arc;

use anyhow::{Context, Result};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};

/// Build a Quinn client endpoint with mTLS.
///
/// - `device_cert_der` + `device_key_der`: always presented (device identity)
/// - `user_cert_der`: presented if enrolled (user association cert)
///
/// Server TLS verification is disabled in dev mode (self-signed server cert).
/// In production, use server_pin for TOFU verification.
pub fn build_client_endpoint(
    device_cert_der: &[u8],
    device_key_der: &[u8],
    user_cert_der: Option<&[u8]>,
) -> Result<quinn::Endpoint> {
    let mut certs = Vec::new();

    // Device cert is always first
    certs.push(CertificateDer::from(device_cert_der.to_vec()));

    // User association cert (if enrolled) goes second
    if let Some(user_cert) = user_cert_der {
        certs.push(CertificateDer::from(user_cert.to_vec()));
    }

    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(device_key_der.to_vec()));

    let tls_config = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(InsecureServerCertVerifier))
        .with_client_auth_cert(certs, key)
        .context("failed to build TLS client config")?;

    let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse()?)?;
    endpoint.set_default_client_config(quinn::ClientConfig::new(Arc::new(
        quinn::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .context("failed to build QUIC client config")?,
    )));

    Ok(endpoint)
}

/// Connect to the server. Returns the QUIC connection.
pub async fn connect(
    endpoint: &quinn::Endpoint,
    server_addr: &str,
) -> Result<quinn::Connection> {
    let addr = server_addr
        .parse()
        .context("invalid server address")?;

    let connection = endpoint
        .connect(addr, "ghostcam")
        .context("failed to initiate QUIC connection")?
        .await
        .context("QUIC connection handshake failed")?;

    Ok(connection)
}

/// Server cert verifier that accepts any certificate (development/self-signed).
#[derive(Debug)]
struct InsecureServerCertVerifier;

impl rustls::client::danger::ServerCertVerifier for InsecureServerCertVerifier {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &rustls::pki_types::ServerName<'_>,
        _ocsp_response: &[u8],
        _now: rustls::pki_types::UnixTime,
    ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn verify_tls13_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        vec![
            rustls::SignatureScheme::ECDSA_NISTP256_SHA256,
            rustls::SignatureScheme::ECDSA_NISTP384_SHA384,
            rustls::SignatureScheme::ED25519,
            rustls::SignatureScheme::RSA_PSS_SHA256,
            rustls::SignatureScheme::RSA_PSS_SHA384,
            rustls::SignatureScheme::RSA_PSS_SHA512,
        ]
    }
}
