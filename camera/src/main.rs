mod capture;
mod config;
mod firmware;
mod http_client;
mod network;
mod provisioning;
mod qr_enrollment;
mod recording;
mod telemetry;
mod upload;

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use bytes::Bytes;
use clap::Parser;
use ghostcam::api_types::CameraCommand;
use ghostcam::config::TELEMETRY_POLL_INTERVAL_SECS;
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use crate::capture::CaptureMessage;
use crate::config::CameraConfig;
use crate::http_client::CameraHttpClient;
use crate::recording::muxer::Muxer;
use crate::recording::ring_buffer::RingBuffer;
use crate::upload::UploadQueue;

#[derive(Parser)]
#[command(name = "ghostcam-camera")]
pub struct Cli {
    /// Path to TOML config file
    #[arg(long)]
    pub config: Option<String>,

    /// Server QUIC address (host:port) — legacy, prefer server_url
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

    /// Disable TOFU server fingerprint verification (legacy)
    #[arg(long)]
    pub no_tofu: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt::init();

    let cli = Cli::parse();
    let camera_config = CameraConfig::load(&cli)?;

    // Ensure directories exist
    std::fs::create_dir_all(&camera_config.data_dir)?;
    std::fs::create_dir_all(&camera_config.segment_dir)?;

    let data_dir = PathBuf::from(&camera_config.data_dir);
    let segment_dir = PathBuf::from(&camera_config.segment_dir);
    let cancel = CancellationToken::new();

    // Get device serial (Pi serial from /proc/cpuinfo, or generate one)
    let device_serial = get_device_serial(&data_dir);
    tracing::info!(serial = %device_serial, "device identity");

    // Check for existing credentials (API key from provisioning)
    let credentials = http_client::load_credentials(&data_dir);

    let (api_key, device_id, server_url) = match credentials {
        Some((key, id, url)) => {
            tracing::info!(device_id = %id, server = %url, "loaded credentials");
            (key, id, url)
        }
        None => {
            // Enter provisioning mode
            tracing::info!("no credentials found, entering provisioning mode");
            provisioning::run_provisioning(&data_dir, &device_serial, cancel.clone()).await?
        }
    };

    // Build HTTP client
    let client = Arc::new(CameraHttpClient::new(&server_url, &api_key, &device_id));

    // Start capture pipeline
    let capture_rx = capture::start_capture(&camera_config, cancel.clone()).await?;

    // Broadcast channels: video NALs and audio frames fan out to the muxer.
    // (No QUIC stream writers in the new design — muxer is the only consumer.)
    let (video_broadcast_tx, _) = broadcast::channel::<Bytes>(512);
    let (audio_broadcast_tx, _) = broadcast::channel::<Bytes>(512);

    let muxer_video_rx = video_broadcast_tx.subscribe();
    let muxer_audio_rx = audio_broadcast_tx.subscribe();

    // Fan out capture messages to broadcast channels
    let fanout_video_tx = video_broadcast_tx.clone();
    let fanout_audio_tx = audio_broadcast_tx.clone();
    let fanout_cancel = cancel.clone();
    tokio::spawn(async move {
        fanout_capture(capture_rx, fanout_video_tx, fanout_audio_tx, fanout_cancel).await;
    });

    // Segment event channel (muxer → upload loop)
    let (seg_event_tx, seg_event_rx) = mpsc::channel(64);

    // Ring buffer (manages on-disk segments)
    let ring_buffer = Arc::new(RwLock::new(
        RingBuffer::scan(&segment_dir, seg_event_tx.clone()).await?,
    ));

    // Muxer task — writes fMP4 segments to disk
    let mut muxer = Muxer::new(
        segment_dir.clone(),
        device_id.clone(),
        seg_event_tx,
        ring_buffer.clone(),
    );
    let muxer_cancel = cancel.clone();
    tokio::spawn(async move {
        if let Err(e) = muxer.run(muxer_video_rx, muxer_audio_rx, muxer_cancel).await {
            tracing::error!("muxer ended: {e}");
        }
    });

    // Upload queue + upload loop
    let upload_queue = Arc::new(UploadQueue::new(segment_dir.clone(), 500));
    let init_segment_path = segment_dir.join("init.mp4");
    let upload_client = client.clone();
    let upload_queue_ref = upload_queue.clone();
    let upload_cancel = cancel.clone();
    tokio::spawn(async move {
        upload::run_upload_loop(
            upload_client,
            upload_queue_ref,
            seg_event_rx,
            init_segment_path,
            upload_cancel,
        )
        .await;
    });

    // Telemetry poll loop (HTTP POST every 10s)
    let telem_client = client.clone();
    let telem_config = CameraConfig {
        server_addr: String::new(),
        test_source: camera_config.test_source,
        test_video: String::new(),
        segment_dir: String::new(),
        no_audio: camera_config.no_audio,
        audio_device: None,
        no_gps: camera_config.no_gps,
        no_tofu: true,
        data_dir: camera_config.data_dir.clone(),
        video_width: camera_config.video_width,
        video_height: camera_config.video_height,
        video_fps: camera_config.video_fps,
        video_bitrate: camera_config.video_bitrate,
        video_keyframe_interval: camera_config.video_keyframe_interval,
    };
    let telem_cancel = cancel.clone();
    tokio::spawn(async move {
        run_telemetry_poll(telem_client, &telem_config, telem_cancel).await;
    });

    // Periodic RSS memory logging (Linux only, every 60s)
    #[cfg(target_os = "linux")]
    {
        let mem_cancel = cancel.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(Duration::from_secs(60));
            interval.tick().await;
            loop {
                tokio::select! {
                    _ = mem_cancel.cancelled() => break,
                    _ = interval.tick() => {
                        if let Ok(statm) = tokio::fs::read_to_string("/proc/self/statm").await {
                            let fields: Vec<&str> = statm.split_whitespace().collect();
                            if let Some(rss_pages) = fields.get(1).and_then(|s| s.parse::<u64>().ok()) {
                                let rss_mb = (rss_pages * 4096) / (1024 * 1024);
                                tracing::info!(rss_mb, "memory usage");
                            }
                        }
                    }
                }
            }
        });
    }

    // Shutdown signal handler
    tokio::spawn(shutdown_signal(cancel.clone()));

    // Wait for cancellation
    cancel.cancelled().await;
    tracing::info!("goodbye");
    Ok(())
}

/// Telemetry poll loop: POST telemetry every 10s, process commands.
async fn run_telemetry_poll(
    client: Arc<CameraHttpClient>,
    config: &CameraConfig,
    cancel: CancellationToken,
) {
    use crate::telemetry::sensors;

    let mut interval = tokio::time::interval(Duration::from_secs(TELEMETRY_POLL_INTERVAL_SECS));
    let mut gps = sensors::GpsSource::new(config).await;

    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = interval.tick() => {
                let telemetry = sensors::read_sensors(config, &mut gps);

                match client.post_telemetry(telemetry).await {
                    Ok(commands) => {
                        for cmd in commands {
                            handle_command(&cmd);
                        }
                    }
                    Err(e) => {
                        tracing::debug!("telemetry POST failed: {e}");
                    }
                }
            }
        }
    }
}

/// Handle a command received from the server via telemetry poll response.
fn handle_command(cmd: &CameraCommand) {
    match cmd {
        CameraCommand::Reboot => {
            tracing::info!("reboot command received");
            std::process::exit(0);
        }
        CameraCommand::Unregister => {
            tracing::info!("unregister command received");
            // TODO: clear credentials and re-enter provisioning
        }
        CameraCommand::SetRecordingMode { mode } => {
            tracing::info!(mode = %mode, "recording mode change requested");
            // TODO: update config
        }
        CameraCommand::SetResolution { resolution } => {
            tracing::info!(resolution = %resolution, "resolution change requested");
            // TODO: update config
        }
        CameraCommand::NetworkConfig { ssid, psk } => {
            tracing::info!(ssid = %ssid, "network config command");
            let ssid = ssid.clone();
            let psk = psk.clone();
            tokio::spawn(async move {
                if let Err(e) = crate::network::ensure_wifi(&ssid, Some(&psk)).await {
                    tracing::warn!("WiFi config failed: {e}");
                }
            });
        }
        CameraCommand::RemoveNetwork { ssid } => {
            tracing::info!(ssid = %ssid, "remove network command");
        }
    }
}

/// Fan out capture messages to broadcast channels for the muxer.
async fn fanout_capture(
    mut rx: mpsc::Receiver<CaptureMessage>,
    video_tx: broadcast::Sender<Bytes>,
    audio_tx: broadcast::Sender<Bytes>,
    cancel: CancellationToken,
) {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            msg = rx.recv() => {
                match msg {
                    Some(CaptureMessage::VideoNal(data)) => {
                        let _ = video_tx.send(data);
                    }
                    Some(CaptureMessage::AudioFrame(data)) => {
                        let _ = audio_tx.send(data);
                    }
                    None => break,
                }
            }
        }
    }
}

/// Read Pi serial number from /proc/cpuinfo (Linux) or generate a random one.
fn get_device_serial(data_dir: &Path) -> String {
    // Check if we already have a stored serial
    let serial_path = data_dir.join("device_serial");
    if let Ok(serial) = std::fs::read_to_string(&serial_path) {
        let serial = serial.trim().to_string();
        if !serial.is_empty() {
            return serial;
        }
    }

    // Try to read Pi serial from /proc/cpuinfo
    #[cfg(target_os = "linux")]
    {
        if let Ok(cpuinfo) = std::fs::read_to_string("/proc/cpuinfo") {
            for line in cpuinfo.lines() {
                if let Some(serial) = line.strip_prefix("Serial") {
                    let serial = serial.trim_start_matches(|c: char| c == ':' || c.is_whitespace());
                    if !serial.is_empty() {
                        let _ = std::fs::write(&serial_path, serial);
                        return serial.to_string();
                    }
                }
            }
        }
    }

    // Fallback: generate a random serial and persist it
    let serial = uuid::Uuid::new_v4().to_string();
    let _ = std::fs::write(&serial_path, &serial);
    serial
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
