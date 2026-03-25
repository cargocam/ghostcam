use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use ghostcam::config::KEEPALIVE_INTERVAL_SECS;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::danger::{ClientCertVerified, ClientCertVerifier};
use rustls::DistinguishedName;

/// A client cert verifier that accepts any certificate.
/// Application-layer code handles enrollment vs normal routing based on cert chain contents.
#[derive(Debug)]
struct AcceptAnyClientCert;

impl ClientCertVerifier for AcceptAnyClientCert {
    fn root_hint_subjects(&self) -> &[DistinguishedName] {
        &[]
    }

    fn verify_client_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _now: rustls::pki_types::UnixTime,
    ) -> Result<ClientCertVerified, rustls::Error> {
        Ok(ClientCertVerified::assertion())
    }

    fn offer_client_auth(&self) -> bool {
        true
    }

    fn client_auth_mandatory(&self) -> bool {
        true
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
            rustls::SignatureScheme::RSA_PKCS1_SHA256,
            rustls::SignatureScheme::RSA_PKCS1_SHA384,
            rustls::SignatureScheme::RSA_PKCS1_SHA512,
        ]
    }
}

/// Build a Quinn server endpoint with mTLS.
pub fn build_server_endpoint(
    server_cert_der: &[u8],
    server_key_der: &[u8],
    bind_addr: SocketAddr,
) -> Result<quinn::Endpoint> {
    let cert = CertificateDer::from(server_cert_der.to_vec());
    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(server_key_der.to_vec()));

    let tls_config = rustls::ServerConfig::builder()
        .with_client_cert_verifier(Arc::new(AcceptAnyClientCert))
        .with_single_cert(vec![cert], key)
        .context("failed to build TLS server config")?;

    let mut transport = quinn::TransportConfig::default();
    transport.keep_alive_interval(Some(Duration::from_secs(KEEPALIVE_INTERVAL_SECS)));
    transport.max_idle_timeout(Some(
        Duration::from_secs(KEEPALIVE_INTERVAL_SECS * 3)
            .try_into()
            .unwrap(),
    ));
    // Enable QUIC datagram support (RFC 9221) so cameras can send telemetry datagrams.
    // Without this, max_datagram_frame_size is not advertised and send_datagram() fails.
    transport.datagram_receive_buffer_size(Some(65536));

    let mut server_config = quinn::ServerConfig::with_crypto(Arc::new(
        quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
            .context("failed to build QUIC server config")?,
    ));
    server_config.transport_config(Arc::new(transport));

    let endpoint = quinn::Endpoint::server(server_config, bind_addr)
        .context("failed to bind QUIC endpoint")?;

    Ok(endpoint)
}
