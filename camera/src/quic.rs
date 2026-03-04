use anyhow::Result;
use quinn::{Connection, RecvStream, SendStream};

/// Connect to bridge, returning the connection and a bidirectional control stream.
pub async fn connect(
    addr: &str,
) -> Result<(Connection, SendStream, RecvStream)> {
    let addr: std::net::SocketAddr = addr.parse()?;

    let (cert_der, key_der) = ghostcam::quic::generate_self_signed_cert("camera")?;
    let client_config = ghostcam::quic::create_quic_client_config(cert_der, key_der)?;

    let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse()?)?;
    endpoint.set_default_client_config(client_config);

    let connection = endpoint.connect(addr, "server")?.await?;

    // Open bidirectional control stream
    let (send, recv) = connection.open_bi().await?;

    Ok((connection, send, recv))
}
