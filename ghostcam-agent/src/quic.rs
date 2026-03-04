use anyhow::Result;
use quinn::{Connection, RecvStream, SendStream};
use rustls::pki_types::{CertificateDer, PrivatePkcs8KeyDer};
use std::sync::Arc;

/// Connect to bridge, returning the connection and a bidirectional control stream.
pub async fn connect(
    addr: &str,
) -> Result<(Connection, SendStream, RecvStream)> {
    let addr: std::net::SocketAddr = addr.parse()?;

    // Self-signed cert for the client (not strictly needed, but quinn requires a config)
    let key_pair = rcgen::KeyPair::generate()?;
    let cert_params = rcgen::CertificateParams::new(vec!["ghostcam-agent".into()])?;
    let cert = cert_params.self_signed(&key_pair)?;
    let cert_der = CertificateDer::from(cert.der().to_vec());
    let key_der = PrivatePkcs8KeyDer::from(key_pair.serialize_der());

    // Skip server certificate verification for dev
    let mut crypto = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(SkipServerVerification))
        .with_client_auth_cert(vec![cert_der], key_der.into())?;
    crypto.alpn_protocols = vec![b"ghostcam".to_vec()];

    let client_config = quinn::ClientConfig::new(Arc::new(
        quinn::crypto::rustls::QuicClientConfig::try_from(crypto)?,
    ));

    let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse()?)?;
    endpoint.set_default_client_config(client_config);

    let connection = endpoint.connect(addr, "ghostcam-bridge")?.await?;

    // Open bidirectional control stream
    let (send, recv) = connection.open_bi().await?;

    Ok((connection, send, recv))
}

/// Accepts any server certificate (dev only).
#[derive(Debug)]
struct SkipServerVerification;

impl rustls::client::danger::ServerCertVerifier for SkipServerVerification {
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
        rustls::crypto::ring::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}
