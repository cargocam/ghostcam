use crate::AppState;
use anyhow::Result;
use ghostcam_common::frame::Frame;
use ghostcam_common::hello::DeviceHello;
use quinn::Endpoint;
use rustls::pki_types::{CertificateDer, PrivatePkcs8KeyDer};
use std::net::SocketAddr;
use std::sync::Arc;
use tracing::{info, warn};

pub async fn run_quic_listener(port: u16, state: Arc<AppState>) -> Result<()> {
    let addr: SocketAddr = format!("0.0.0.0:{port}").parse()?;

    // Self-signed cert for QUIC server
    let key_pair = rcgen::KeyPair::generate()?;
    let cert_params = rcgen::CertificateParams::new(vec!["ghostcam-bridge".into()])?;
    let cert = cert_params.self_signed(&key_pair)?;
    let cert_der = CertificateDer::from(cert.der().to_vec());
    let key_der = PrivatePkcs8KeyDer::from(key_pair.serialize_der());

    let mut server_crypto = rustls::ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(vec![cert_der], key_der.into())?;
    server_crypto.alpn_protocols = vec![b"ghostcam".to_vec()];

    let server_config = quinn::ServerConfig::with_crypto(Arc::new(
        quinn::crypto::rustls::QuicServerConfig::try_from(server_crypto)?,
    ));

    let endpoint = Endpoint::server(server_config, addr)?;
    info!(addr = %addr, "QUIC listener started");

    while let Some(incoming) = endpoint.accept().await {
        let state = state.clone();
        tokio::spawn(async move {
            match incoming.await {
                Ok(conn) => {
                    if let Err(e) = handle_camera_connection(conn, state).await {
                        warn!(error = %e, "camera connection error");
                    }
                }
                Err(e) => warn!(error = %e, "failed to accept connection"),
            }
        });
    }

    Ok(())
}

async fn handle_camera_connection(
    connection: quinn::Connection,
    state: Arc<AppState>,
) -> Result<()> {
    let remote = connection.remote_address();
    info!(remote = %remote, "camera connected");

    // Read hello from bidirectional control stream
    let (_, mut control_recv) = connection.accept_bi().await?;

    // Read length-prefixed JSON hello
    let mut len_buf = [0u8; 4];
    control_recv.read_exact(&mut len_buf).await?;
    let len = u32::from_be_bytes(len_buf) as usize;

    let mut hello_buf = vec![0u8; len];
    control_recv.read_exact(&mut hello_buf).await?;
    let hello: DeviceHello = serde_json::from_slice(&hello_buf)?;

    info!(
        device_id = %hello.device_id,
        group_id = %hello.group_id,
        capabilities = ?hello.capabilities,
        "received device hello"
    );

    // Register camera
    let device_id = hello.device_id.clone();
    {
        let mut router = state.router.write().await;
        router.register_camera(
            hello.device_id.clone(),
            hello.group_id.clone(),
            hello.capabilities.clone(),
        );
    }

    // Accept unidirectional streams (each carries one frame)
    let state_for_frames = state.clone();
    let device_id_for_frames = device_id.clone();
    loop {
        match connection.accept_uni().await {
            Ok(mut recv) => {
                let state = state_for_frames.clone();
                let device_id = device_id_for_frames.clone();
                tokio::spawn(async move {
                    if let Err(e) = read_frame_stream(&mut recv, &state, &device_id).await {
                        // ReadError on stream finish is normal
                        if !e.to_string().contains("finished") {
                            warn!(device_id = %device_id, error = %e, "frame stream error");
                        }
                    }
                });
            }
            Err(e) => {
                info!(device_id = %device_id, error = %e, "camera disconnected");
                break;
            }
        }
    }

    // Unregister camera
    {
        let mut router = state.router.write().await;
        router.unregister_camera(&device_id);
    }

    Ok(())
}

async fn read_frame_stream(
    recv: &mut quinn::RecvStream,
    state: &AppState,
    device_id: &str,
) -> Result<()> {
    // Read entire stream (one frame per uni stream)
    let data = recv.read_to_end(1024 * 1024).await?; // 1MB max
    let frame = Frame::decode(&data)?;

    match frame.stream_type {
        ghostcam_common::frame::StreamType::Video => {
            let mut router = state.router.write().await;
            router.on_video_frame(device_id, frame.timestamp_us, frame.payload);
        }
        ghostcam_common::frame::StreamType::Audio => {
            let router = state.router.read().await;
            router.on_audio_frame(device_id, frame.timestamp_us, frame.payload);
        }
    }

    Ok(())
}
