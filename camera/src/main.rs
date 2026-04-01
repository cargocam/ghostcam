mod config;
mod firmware;
mod http_client;
mod network;
mod provisioning;
mod qr_enrollment;
mod telemetry;
mod upload;

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use clap::Parser;
use ghostcam::api_types::CameraCommand;
use ghostcam::config::TELEMETRY_POLL_INTERVAL_SECS;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::config::CameraConfig;
use crate::http_client::CameraHttpClient;
use crate::upload::UploadQueue;

/// Segment duration in seconds (must match ffmpeg -segment_time).
const SEGMENT_DURATION_SECS: u64 = 6;

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

    /// Directory for segment ring buffer
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

    // Spawn the capture pipeline (rpicam-vid | ffmpeg, or ffmpeg test source)
    let pipeline_cancel = cancel.clone();
    let pipeline_segment_dir = segment_dir.clone();
    let pipeline_config = CaptureConfig {
        test_source: camera_config.test_source,
        video_width: camera_config.video_width,
        video_height: camera_config.video_height,
        video_fps: camera_config.video_fps,
        video_bitrate: camera_config.video_bitrate,
        video_keyframe_interval: camera_config.video_keyframe_interval,
    };
    tokio::spawn(async move {
        if let Err(e) =
            spawn_capture_pipeline(&pipeline_config, &pipeline_segment_dir, pipeline_cancel).await
        {
            tracing::error!("capture pipeline failed: {e}");
        }
    });

    // Segment watcher channel (watcher → upload loop)
    let (seg_tx, seg_rx) = mpsc::channel(64);

    // Segment directory watcher
    let watcher_cancel = cancel.clone();
    let watcher_dir = segment_dir.clone();
    tokio::spawn(async move {
        run_segment_watcher(&watcher_dir, seg_tx, watcher_cancel).await;
    });

    // Upload queue + upload loop
    let upload_queue = Arc::new(UploadQueue::new(segment_dir.clone(), 500));
    let upload_client = client.clone();
    let upload_queue_ref = upload_queue.clone();
    let upload_cancel = cancel.clone();
    tokio::spawn(async move {
        upload::run_upload_loop(upload_client, upload_queue_ref, seg_rx, upload_cancel).await;
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

/// Subset of CameraConfig needed by the capture pipeline.
struct CaptureConfig {
    test_source: bool,
    video_width: u32,
    video_height: u32,
    video_fps: u32,
    video_bitrate: u32,
    video_keyframe_interval: u32,
}

/// Spawn the capture pipeline that writes MPEG-TS segments to `segment_dir`.
///
/// For real hardware: `rpicam-vid | ffmpeg` piped together.
/// For test-source mode: `ffmpeg` with testsrc2 input.
///
/// Blocks until the pipeline exits or cancellation is signalled.
async fn spawn_capture_pipeline(
    config: &CaptureConfig,
    segment_dir: &Path,
    cancel: CancellationToken,
) -> Result<()> {
    let segment_pattern = segment_dir.join("seg%05d.ts");
    let pattern_str = segment_pattern.to_string_lossy().to_string();

    // Keyframe interval should align with segment duration for clean cuts
    let keyframe_interval = config.video_keyframe_interval;
    let gop_str = format!("keyint={}:min-keyint={}", keyframe_interval, keyframe_interval);

    if config.test_source {
        tracing::info!("starting test capture pipeline (ffmpeg testsrc2)");
        let size = format!("{}x{}", config.video_width, config.video_height);
        let input = format!(
            "testsrc2=size={}:rate={}",
            size, config.video_fps
        );

        let mut child = tokio::process::Command::new("ffmpeg")
            .args([
                "-re",
                "-f", "lavfi",
                "-i", &input,
                "-c:v", "libx264",
                "-preset", "ultrafast",
                "-x264-params", &gop_str,
                "-f", "segment",
                "-segment_time", &SEGMENT_DURATION_SECS.to_string(),
                "-reset_timestamps", "1",
                &pattern_str,
            ])
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .spawn()?;

        tracing::info!("ffmpeg test pipeline started (pid={})", child.id().unwrap_or(0));

        tokio::select! {
            _ = cancel.cancelled() => {
                tracing::info!("cancelling capture pipeline");
                kill_process(&mut child).await;
            }
            status = child.wait() => {
                match status {
                    Ok(s) => tracing::warn!("ffmpeg exited: {s}"),
                    Err(e) => tracing::error!("ffmpeg wait failed: {e}"),
                }
            }
        }
    } else {
        tracing::info!("starting real capture pipeline (rpicam-vid | ffmpeg)");

        // Use a shell pipe to connect rpicam-vid stdout to ffmpeg stdin.
        // This avoids needing to manually wire async fd ownership across processes.
        let pipeline_cmd = format!(
            "rpicam-vid --codec h264 --inline --width {} --height {} --framerate {} --bitrate {} -t 0 -o - 2>/dev/null | \
             ffmpeg -loglevel warning -analyzeduration 1M -probesize 1M -f h264 -framerate {} -i pipe:0 \
             -c:v copy -f segment -segment_time {} -segment_format mpegts -reset_timestamps 1 {} 2>/dev/null",
            config.video_width,
            config.video_height,
            config.video_fps,
            config.video_bitrate,
            config.video_fps,
            SEGMENT_DURATION_SECS,
            shell_escape(&pattern_str),
        );

        let mut child = tokio::process::Command::new("sh")
            .args(["-c", &pipeline_cmd])
            .stdin(std::process::Stdio::null())
            .spawn()?;

        tracing::info!(pid = child.id().unwrap_or(0), "real capture pipeline started");

        tokio::select! {
            _ = cancel.cancelled() => {
                tracing::info!("cancelling capture pipeline");
                kill_process(&mut child).await;
            }
            status = child.wait() => {
                match status {
                    Ok(s) => tracing::warn!("capture pipeline exited: {s}"),
                    Err(e) => tracing::error!("capture pipeline wait failed: {e}"),
                }
            }
        }
    }

    Ok(())
}

/// Escape a string for use in a shell command.
fn shell_escape(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// Kill a child process gracefully (SIGTERM on Unix, kill on other platforms).
async fn kill_process(child: &mut tokio::process::Child) {
    if let Err(e) = child.kill().await {
        tracing::debug!("kill failed (process may have already exited): {e}");
    }
}

/// A new `.ts` segment file was detected by the watcher.
pub struct NewSegment {
    pub filename: String,
    pub path: PathBuf,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
}

/// Poll the segment directory every 2 seconds for new `.ts` files.
/// When a new file appears, send it to the upload channel.
async fn run_segment_watcher(
    segment_dir: &Path,
    tx: mpsc::Sender<NewSegment>,
    cancel: CancellationToken,
) {
    let mut known_files: HashSet<String> = HashSet::new();
    let mut interval = tokio::time::interval(Duration::from_secs(2));

    // Scan existing files on startup so we don't re-upload old segments
    if let Ok(mut entries) = tokio::fs::read_dir(segment_dir).await {
        while let Ok(Some(entry)) = entries.next_entry().await {
            if let Some(name) = entry.file_name().to_str() {
                if name.ends_with(".ts") {
                    known_files.insert(name.to_string());
                }
            }
        }
    }
    tracing::info!(
        existing = known_files.len(),
        "segment watcher started, ignoring existing files"
    );

    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = interval.tick() => {
                let mut entries = match tokio::fs::read_dir(segment_dir).await {
                    Ok(e) => e,
                    Err(e) => {
                        tracing::warn!("failed to read segment dir: {e}");
                        continue;
                    }
                };

                let mut new_files: Vec<(String, PathBuf, u64, u64)> = Vec::new();

                while let Ok(Some(entry)) = entries.next_entry().await {
                    let name = match entry.file_name().to_str().map(String::from) {
                        Some(n) if n.ends_with(".ts") => n,
                        _ => continue,
                    };

                    if known_files.contains(&name) {
                        continue;
                    }

                    let path = entry.path();
                    let meta = match entry.metadata().await {
                        Ok(m) => m,
                        Err(_) => continue,
                    };

                    // Use file modification time as the segment start timestamp
                    let mtime_ms = meta
                        .modified()
                        .ok()
                        .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
                        .map(|d| d.as_millis() as u64)
                        .unwrap_or(0);

                    let size = meta.len();

                    // Skip empty files — ffmpeg creates the file before writing data.
                    // Also skip files modified less than 2s ago (still being written).
                    if size == 0 {
                        continue;
                    }
                    let now_ms = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap()
                        .as_millis() as u64;
                    if now_ms.saturating_sub(mtime_ms) < 2000 {
                        continue;
                    }
                    new_files.push((name, path, mtime_ms, size));
                }

                // Sort by filename (sequential counter ensures chronological order)
                new_files.sort_by(|a, b| a.0.cmp(&b.0));

                for (name, path, mtime_ms, size) in new_files {
                    // Derive start_ts from mtime, end_ts from segment duration
                    let start_ts = mtime_ms.saturating_sub(SEGMENT_DURATION_SECS * 1000);
                    let end_ts = mtime_ms;

                    tracing::debug!(
                        file = %name,
                        size_bytes = size,
                        "new segment detected"
                    );

                    known_files.insert(name.clone());

                    let seg = NewSegment {
                        filename: name,
                        path,
                        start_ts,
                        end_ts,
                        size_bytes: size,
                    };

                    if tx.send(seg).await.is_err() {
                        tracing::warn!("upload channel closed, stopping watcher");
                        return;
                    }
                }
            }
        }
    }
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
