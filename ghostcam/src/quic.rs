use anyhow::Result;
use quinn::SendStream;
use rustls::pki_types::{CertificateDer, PrivatePkcs8KeyDer};
use std::sync::Arc;

use crate::hello::DeviceHello;

/// Generate a self-signed certificate for QUIC transport (dev only).
pub fn generate_self_signed_cert(
    san: &str,
) -> Result<(CertificateDer<'static>, PrivatePkcs8KeyDer<'static>)> {
    let key_pair = rcgen::KeyPair::generate()?;
    let cert_params = rcgen::CertificateParams::new(vec![san.into()])?;
    let cert = cert_params.self_signed(&key_pair)?;
    let cert_der = CertificateDer::from(cert.der().to_vec());
    let key_der = PrivatePkcs8KeyDer::from(key_pair.serialize_der());
    Ok((cert_der, key_der))
}

/// Create a QUIC server config with the given certificate.
pub fn create_quic_server_config(
    cert_der: CertificateDer<'static>,
    key_der: PrivatePkcs8KeyDer<'static>,
) -> Result<quinn::ServerConfig> {
    let mut server_crypto = rustls::ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(vec![cert_der], key_der.into())?;
    server_crypto.alpn_protocols = vec![b"ghostcam".to_vec()];

    let server_config = quinn::ServerConfig::with_crypto(Arc::new(
        quinn::crypto::rustls::QuicServerConfig::try_from(server_crypto)?,
    ));
    Ok(server_config)
}

/// Create a QUIC client config that skips server verification (dev only).
pub fn create_quic_client_config(
    cert_der: CertificateDer<'static>,
    key_der: PrivatePkcs8KeyDer<'static>,
) -> Result<quinn::ClientConfig> {
    let mut crypto = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(SkipServerVerification))
        .with_client_auth_cert(vec![cert_der], key_der.into())?;
    crypto.alpn_protocols = vec![b"ghostcam".to_vec()];

    let client_config = quinn::ClientConfig::new(Arc::new(
        quinn::crypto::rustls::QuicClientConfig::try_from(crypto)?,
    ));
    Ok(client_config)
}

/// Send a length-prefixed JSON DeviceHello on the control stream.
pub async fn send_hello(stream: &mut SendStream, hello: &DeviceHello) -> Result<()> {
    let hello_json = serde_json::to_vec(hello)?;
    stream
        .write_all(&(hello_json.len() as u32).to_be_bytes())
        .await?;
    stream.write_all(&hello_json).await?;
    Ok(())
}

/// Read a length-prefixed JSON DeviceHello from the control stream.
pub async fn recv_hello(recv: &mut quinn::RecvStream) -> Result<DeviceHello> {
    let mut len_buf = [0u8; 4];
    recv.read_exact(&mut len_buf).await?;
    let len = u32::from_be_bytes(len_buf) as usize;

    let mut hello_buf = vec![0u8; len];
    recv.read_exact(&mut hello_buf).await?;
    let hello: DeviceHello = serde_json::from_slice(&hello_buf)?;
    Ok(hello)
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
