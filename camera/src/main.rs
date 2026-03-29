mod capture;
mod certs;
mod commands;
mod config;
mod enrollment;
mod firmware;
mod network;
mod quic;
mod recording;
mod session;
mod telemetry;
mod tofu;

use std::path::Path;
use std::time::Duration;

use anyhow::Result;
use clap::Parser;
use ghostcam::config::{RECONNECT_BACKOFF_INITIAL_SECS, RECONNECT_BACKOFF_MAX_SECS};
use tokio::sync::{mpsc, watch};
use tokio_util::sync::CancellationToken;

use crate::capture::CaptureMessage;
use crate::config::CameraConfig;
use crate::network::{spawn_network_monitor, wait_for_route};
use crate::telemetry::buffer::TelemetryBuffer;

/// Timeout for individual frame sends. QUIC connections can hang for 30s+
/// without signaling an error; this ensures we detect dead links quickly.
const SEND_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Parser)]
#[command(name = "ghostcam-camera")]
pub struct Cli {
    /// Path to TOML config file
    #[arg(long)]
    pub config: Option<String>,

    /// Server QUIC address (host:port)
    #[arg(long)]
    pub server_addr: Option<String>,

    /// Use test video + audio sources instead of real capture
    #[arg(long)]
    pub test_source: bool,

    /// Path to test H.264 file
    #[arg(long)]
    pub test_video: Option<String>,

    /// Directory for fMP4 ring buffer
    #[arg(long)]
    pub segment_dir: Option<String>,

    /// Disable audio capture
    #[arg(long)]
    pub no_audio: bool,

    /// Disable GPS even if gpsd is available
    #[arg(long)]
    pub no_gps: bool,

    /// Data directory
    #[arg(long)]
    pub data_dir: Option<String>,

    /// Enrollment JWT (bypasses QR scanning for registration)
    #[arg(long)]
    pub enrollment_jwt: Option<String>,

    /// Disable TOFU server fingerprint verification
    #[arg(long)]
    pub no_tofu: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt::init();

    let cli = Cli::parse();

    let camera_config = CameraConfig::load(&cli)?;

    // Ensure data directory exists
    std::fs::create_dir_all(&camera_config.data_dir)?;

    // Load or create device certificate
    let cert_path = Path::new(&camera_config.data_dir).join("device.crt");
    let key_path = Path::new(&camera_config.data_dir).join("device.key");
    let (device_cert, device_key) = certs::load_or_create_device_cert(&cert_path, &key_path)?;

    // Load user association cert (if enrolled)
    let user_cert_path = Path::new(&camera_config.data_dir).join("user.crt");
    let user_key_path = Path::new(&camera_config.data_dir).join("user.key");
    let mut user_cert = certs::load_user_cert(&user_cert_path, &user_key_path)?;

    let fingerprint = ghostcam::pki::cert_fingerprint(&device_cert);
    tracing::info!(fingerprint = %fingerprint, "device identity loaded");

    // Handle enrollment if not yet enrolled
    if user_cert.is_none() {
        if let Some(jwt) = &cli.enrollment_jwt {
            tracing::info!("enrollment JWT provided via CLI");
            match enrollment::parse_enrollment_jwt(jwt) {
                Ok(enrollment_data) => {
                    match enrollment::enroll(&enrollment_data, &device_cert, &device_key).await {
                        Ok(result) => {
                            enrollment::store_enrollment(
                                Path::new(&camera_config.data_dir),
                                &result,
                                &enrollment_data.server_addr,
                            )
                            .await?;
                            tracing::info!("enrollment complete");
                            // Reload the user cert now that enrollment stored it
                            user_cert = certs::load_user_cert(&user_cert_path, &user_key_path)?;
                        }
                        Err(e) => {
                            tracing::error!("enrollment failed: {e}");
                            return Err(e);
                        }
                    }
                }
                Err(e) => {
                    tracing::error!("invalid enrollment JWT: {e}");
                    return Err(e);
                }
            }
        } else {
            tracing::warn!("no user association certificate — enrollment required");
            tracing::warn!("use --enrollment-jwt to enroll this camera");
        }
    }

    // Load telemetry buffer
    let buffer_path = Path::new(&camera_config.data_dir).join("telemetry.buf");
    let telemetry_buffer = TelemetryBuffer::load(&buffer_path)?;

    let cancel = CancellationToken::new();

    // Start capture pipeline
    let capture_rx = capture::start_capture(&camera_config, cancel.clone()).await?;

    // Fan out capture messages to video and audio channels
    let (video_tx, video_rx) = mpsc::channel::<CaptureMessage>(256);
    let (audio_tx, audio_rx) = mpsc::channel::<CaptureMessage>(256);
    let fanout_cancel = cancel.clone();
    tokio::spawn(fanout_capture(
        capture_rx,
        video_tx,
        audio_tx,
        fanout_cancel,
    ));

    // Telemetry loop with connection watch
    let (conn_tx, conn_rx) = watch::channel::<Option<quinn::Connection>>(None);
    let telem_no_gps = camera_config.no_gps;
    let telem_buffer_path = buffer_path.clone();
    tokio::spawn({
        let cancel = cancel.clone();
        async move {
            let buffer = match TelemetryBuffer::load(&telem_buffer_path) {
                Ok(b) => b,
                Err(e) => {
                    tracing::error!("failed to load telemetry buffer for loop: {e}");
                    return;
                }
            };
            let config = CameraConfig {
                server_addr: String::new(),
                test_source: true,
                test_video: String::new(),
                segment_dir: String::new(),
                no_audio: camera_config.no_audio,
                audio_device: None,
                no_gps: telem_no_gps,
                no_tofu: true,
                data_dir: String::new(),
                video_width: 1280,
                video_height: 720,
                video_fps: 30,
                video_bitrate: 2_000_000,
                video_keyframe_interval: 60,
            };
            if let Err(e) = telemetry::run_telemetry_loop(conn_rx, &buffer, &config, cancel).await {
                tracing::warn!("telemetry loop ended: {e}");
            }
        }
    });

    // Shutdown signal handler
    tokio::spawn(shutdown_signal(cancel.clone()));

    // Network monitor — detects default route changes (Linux only, no-op on macOS)
    let net_change_rx = spawn_network_monitor();

    // Connection loop
    run_connection_loop(
        &camera_config,
        &device_cert,
        &device_key,
        user_cert.as_ref().map(|(c, _)| c.as_slice()),
        &telemetry_buffer,
        &fingerprint.0,
        video_rx,
        audio_rx,
        conn_tx,
        net_change_rx,
        cancel,
    )
    .await?;

    // Flush telemetry buffer on shutdown
    telemetry_buffer.flush_to_disk().await?;
    tracing::info!("goodbye");
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn run_connection_loop(
    config: &CameraConfig,
    device_cert: &[u8],
    device_key: &[u8],
    user_cert: Option<&[u8]>,
    telemetry_buffer: &TelemetryBuffer,
    device_fingerprint: &str,
    video_rx: mpsc::Receiver<CaptureMessage>,
    audio_rx: mpsc::Receiver<CaptureMessage>,
    conn_tx: watch::Sender<Option<quinn::Connection>>,
    net_change_rx: watch::Receiver<u64>,
    cancel: CancellationToken,
) -> Result<()> {
    let mut backoff = Duration::from_secs(RECONNECT_BACKOFF_INITIAL_SECS);
    let mut video_rx = video_rx;
    let mut audio_rx = audio_rx;
    let mut net_change_rx = net_change_rx;

    loop {
        if cancel.is_cancelled() {
            break;
        }

        // Wait for a default route before attempting to connect.
        // After WiFi drops, cellular may take 10-30s to come up.
        wait_for_route().await;

        tracing::info!(addr = %config.server_addr, "connecting to server");

        // Mark current network state as seen — only react to NEW changes.
        net_change_rx.borrow_and_update();

        match try_connect_and_run(
            config,
            device_cert,
            device_key,
            user_cert,
            telemetry_buffer,
            device_fingerprint,
            &mut video_rx,
            &mut audio_rx,
            &conn_tx,
            &mut net_change_rx,
            &cancel,
        )
        .await
        {
            Ok(()) => break,
            Err(e) => {
                let _ = conn_tx.send(None);

                // Drain buffered frames during reconnection to prevent
                // backpressure on the capture pipeline.
                while video_rx.try_recv().is_ok() {}
                while audio_rx.try_recv().is_ok() {}

                tracing::warn!("connection lost: {e}");
                tracing::info!("reconnecting in {:?}", backoff);
                tokio::select! {
                    _ = tokio::time::sleep(backoff) => {}
                    _ = cancel.cancelled() => break,
                }
                backoff = (backoff * 2).min(Duration::from_secs(RECONNECT_BACKOFF_MAX_SECS));
            }
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn try_connect_and_run(
    config: &CameraConfig,
    device_cert: &[u8],
    device_key: &[u8],
    user_cert: Option<&[u8]>,
    telemetry_buffer: &TelemetryBuffer,
    device_fingerprint: &str,
    video_rx: &mut mpsc::Receiver<CaptureMessage>,
    audio_rx: &mut mpsc::Receiver<CaptureMessage>,
    conn_tx: &watch::Sender<Option<quinn::Connection>>,
    net_change_rx: &mut watch::Receiver<u64>,
    cancel: &CancellationToken,
) -> Result<()> {
    let endpoint = quic::build_client_endpoint(
        device_cert,
        device_key,
        user_cert,
        config.no_tofu,
        std::path::Path::new(&config.data_dir),
    )?;
    let connection = quic::connect(&endpoint, &config.server_addr).await?;

    tracing::info!("connected to server");
    let _ = conn_tx.send(Some(connection.clone()));

    let session_cancel = cancel.child_token();
    let data_dir = std::path::PathBuf::from(&config.data_dir);
    let segment_dir = std::path::PathBuf::from(&config.segment_dir);
    let sess = session::Session::establish(
        connection,
        telemetry_buffer,
        session_cancel,
        data_dir,
        segment_dir,
        device_fingerprint.to_string(),
    )
    .await?;

    // Mark healthy for watchdog
    firmware::mark_healthy(std::path::Path::new(&config.data_dir)).await;

    // Bridge the persistent capture channels into per-session channels.
    let (vid_tx, vid_rx) = mpsc::channel(256);
    let (aud_tx, aud_rx) = mpsc::channel(256);

    let mut sess_handle = tokio::spawn(async move { sess.run(vid_rx, aud_rx).await });

    // Drain loop: forward frames into the session's channels.
    // Exits when the session finishes, the network changes, or a send times out.
    let result = loop {
        tokio::select! {
            _ = cancel.cancelled() => break Ok(()),
            result = &mut sess_handle => {
                break match result {
                    Ok(Ok(_)) => Ok(()),
                    Ok(Err(e)) => Err(e),
                    Err(e) => Err(anyhow::anyhow!("session task panicked: {e}")),
                };
            }
            Ok(_) = net_change_rx.changed() => {
                tracing::warn!("network interface changed, reconnecting");
                break Err(anyhow::anyhow!("network interface changed"));
            }
            msg = video_rx.recv() => {
                if let Some(m) = msg {
                    // Race the channel send against a 5s timeout — dead QUIC
                    // connections can cause channel backpressure for 30s+.
                    tokio::select! {
                        _ = vid_tx.send(m) => {}
                        _ = tokio::time::sleep(SEND_TIMEOUT) => {
                            tracing::warn!("video send timeout ({}s), connection likely dead", SEND_TIMEOUT.as_secs());
                            break Err(anyhow::anyhow!("send timeout"));
                        }
                    }
                } else {
                    break Ok(());
                }
            }
            msg = audio_rx.recv() => {
                if let Some(m) = msg {
                    tokio::select! {
                        _ = aud_tx.send(m) => {}
                        _ = tokio::time::sleep(SEND_TIMEOUT) => {
                            tracing::warn!("audio send timeout ({}s), connection likely dead", SEND_TIMEOUT.as_secs());
                            break Err(anyhow::anyhow!("send timeout"));
                        }
                    }
                } else {
                    break Ok(());
                }
            }
        }
    };
    result
}

/// Fan out capture messages to separate video and audio channels.
async fn fanout_capture(
    mut rx: mpsc::Receiver<CaptureMessage>,
    video_tx: mpsc::Sender<CaptureMessage>,
    audio_tx: mpsc::Sender<CaptureMessage>,
    cancel: CancellationToken,
) {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            msg = rx.recv() => {
                match msg {
                    Some(m @ CaptureMessage::VideoNal(_)) => {
                        let _ = video_tx.send(m).await;
                    }
                    Some(m @ CaptureMessage::AudioFrame(_)) => {
                        let _ = audio_tx.send(m).await;
                    }
                    None => break,
                }
            }
        }
    }
}

async fn shutdown_signal(cancel: CancellationToken) {
    let ctrl_c = tokio::signal::ctrl_c();

    #[cfg(unix)]
    {
        let mut sigterm =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).unwrap();

        tokio::select! {
            _ = ctrl_c => tracing::info!("SIGINT received"),
            _ = sigterm.recv() => tracing::info!("SIGTERM received"),
        }
    }

    #[cfg(not(unix))]
    {
        let _ = ctrl_c.await;
        tracing::info!("SIGINT received");
    }

    cancel.cancel();
}
