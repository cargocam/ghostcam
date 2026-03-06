use crate::AppState;
use anyhow::Result;
use ghostcam::audit::AuditEvent;
use ghostcam::command::CameraCommand;
use ghostcam::frame::Frame;
use quinn::Endpoint;
use std::net::SocketAddr;
use std::sync::Arc;
use tokio::sync::mpsc;
use tracing::{info, warn};

pub async fn run_quic_listener(port: u16, state: Arc<AppState>) -> Result<()> {
    let addr: SocketAddr = format!("0.0.0.0:{port}").parse()?;

    let (cert_der, key_der) = ghostcam::quic::generate_self_signed_cert("server")?;
    let server_config = ghostcam::quic::create_quic_server_config(cert_der, key_der)?;

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
    let (mut control_send, mut control_recv) = connection.accept_bi().await?;
    let hello = ghostcam::quic::recv_hello(&mut control_recv).await?;

    info!(
        device_id = %hello.device_id,
        group_id = %hello.group_id,
        capabilities = ?hello.capabilities,
        "received device hello"
    );

    // Register camera with command channel
    let device_id = hello.device_id.clone();
    let (cmd_tx, mut cmd_rx) = mpsc::channel::<CameraCommand>(64);
    {
        let mut router = state.router.write().await;
        router.register_camera(
            hello.device_id.clone(),
            hello.group_id.clone(),
            hello.capabilities.clone(),
            cmd_tx,
        );
    }

    state.audit.log(AuditEvent::CameraConnect {
        device_id: device_id.clone(),
        group_id: hello.group_id.0.clone(),
        remote_addr: remote.to_string(),
    });
    state.metrics.camera_connections_total.inc();
    state.metrics.active_cameras.inc();

    // Spawn task to forward commands from channel to QUIC control stream
    let cmd_device_id = device_id.clone();
    tokio::spawn(async move {
        while let Some(cmd) = cmd_rx.recv().await {
            info!(device_id = %cmd_device_id, command = ?cmd, "sending command to camera");
            if let Err(e) = ghostcam::quic::send_command(&mut control_send, &cmd).await {
                warn!(device_id = %cmd_device_id, error = %e, "failed to send command to camera");
                break;
            }
        }
    });

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

    state.audit.log(AuditEvent::CameraDisconnect {
        device_id: device_id.clone(),
        reason: "connection closed".into(),
    });
    state.metrics.camera_disconnections_total.inc();
    state.metrics.active_cameras.dec();

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
        ghostcam::frame::StreamType::Video => {
            state.metrics.video_frames_total.inc();
            state.metrics.video_bytes_total.inc_by(frame.payload.len() as u64);
            let mut router = state.router.write().await;
            router.on_video_frame(device_id, frame.timestamp_us, frame.payload);
        }
        ghostcam::frame::StreamType::Audio => {
            state.metrics.audio_frames_total.inc();
            state.metrics.audio_bytes_total.inc_by(frame.payload.len() as u64);
            let router = state.router.read().await;
            router.on_audio_frame(device_id, frame.timestamp_us, frame.payload);
        }
        ghostcam::frame::StreamType::Telemetry => {
            let mut router = state.router.write().await;
            router.on_telemetry_frame(device_id, frame.timestamp_us, frame.payload);
        }
    }

    Ok(())
}
