mod capture;
mod quic;

use anyhow::Result;
use bytes::Bytes;
use capture::CaptureMessage;
use clap::Parser;
use ghostcam::command::CameraCommand;
use ghostcam::h264;
use ghostcam::stream;
use ghostcam::{group::GroupId, hello::DeviceHello};
use std::path::PathBuf;
use std::time::Duration;
use tokio::sync::{mpsc, watch};
use tracing::{info, warn};

#[derive(Parser)]
#[command(name = "camera", about = "Ghostcam camera agent")]
struct Args {
    /// Bridge address (host:port)
    #[arg(long, default_value = "127.0.0.1:4433")]
    bridge_addr: String,

    /// Device ID
    #[arg(long, default_value = "test-cam-01")]
    device_id: String,

    /// Group ID
    #[arg(long, default_value = "default")]
    group_id: String,

    /// Use test source (loop H.264 file + Opus silence) instead of real capture
    #[arg(long)]
    test_source: bool,

    /// Path to raw H.264 annex-B test file (for --test-source)
    #[arg(long, default_value = "test-data/test.h264")]
    test_file: PathBuf,

    /// Video width
    #[arg(long, default_value = "1280")]
    width: u32,

    /// Video height
    #[arg(long, default_value = "720")]
    height: u32,

    /// Target frames per second
    #[arg(long, default_value = "30")]
    fps: u32,

    /// Video bitrate in bits/s (0 = auto)
    #[arg(long, default_value = "0")]
    bitrate: u32,

    /// Keyframe interval in frames
    #[arg(long, default_value = "60")]
    keyframe_interval: u32,

    /// Disable audio capture
    #[arg(long)]
    no_audio: bool,

    /// Disable telemetry collection
    #[arg(long)]
    no_telemetry: bool,

    /// Enable GPS via gpsd
    #[arg(long)]
    enable_gps: bool,
}

const BACKOFF_INITIAL: Duration = Duration::from_secs(1);
const BACKOFF_MAX: Duration = Duration::from_secs(30);
const BACKOFF_MULTIPLIER: u32 = 2;

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "camera=info".into()),
        )
        .init();

    let args = Args::parse();

    info!(
        device_id = %args.device_id,
        group_id = %args.group_id,
        bridge = %args.bridge_addr,
        test_source = args.test_source,
        "starting ghostcam agent"
    );

    // Create capture channel
    let (capture_tx, mut capture_rx) = mpsc::channel::<CaptureMessage>(256);

    // Start capture sources
    if args.test_source {
        start_test_sources(&args, capture_tx)?;
    } else {
        start_real_sources(&args, capture_tx).await?;
    }

    // Signal handling for clean shutdown
    let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    let mut sigint = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())?;

    // Connection loop with exponential backoff
    let mut backoff = BACKOFF_INITIAL;

    loop {
        info!(bridge = %args.bridge_addr, "connecting to bridge");

        tokio::select! {
            result = connect_and_run(&args, &mut capture_rx) => {
                match result {
                    Ok(()) => {
                        info!("session ended cleanly");
                        backoff = BACKOFF_INITIAL;
                    }
                    Err(e) => {
                        warn!(error = %e, "session error");
                    }
                }
            }
            _ = sigterm.recv() => {
                info!("received SIGTERM, shutting down");
                return Ok(());
            }
            _ = sigint.recv() => {
                info!("received SIGINT, shutting down");
                return Ok(());
            }
        }

        info!(delay_secs = backoff.as_secs(), "reconnecting after delay");

        tokio::select! {
            _ = drain_during_delay(&mut capture_rx, backoff) => {}
            _ = sigterm.recv() => {
                info!("received SIGTERM during backoff, shutting down");
                return Ok(());
            }
            _ = sigint.recv() => {
                info!("received SIGINT during backoff, shutting down");
                return Ok(());
            }
        }

        // Exponential backoff
        backoff = (backoff * BACKOFF_MULTIPLIER).min(BACKOFF_MAX);
    }
}

/// Connect to bridge and run the send session until an error occurs.
async fn connect_and_run(
    args: &Args,
    capture_rx: &mut mpsc::Receiver<CaptureMessage>,
) -> Result<()> {
    let (connection, mut control_send, control_recv) =
        quic::connect(&args.bridge_addr).await?;
    info!("connected to bridge");

    // Send hello
    let hello = DeviceHello {
        device_id: args.device_id.clone(),
        group_id: GroupId::new(&args.group_id),
        capabilities: vec!["h264".into(), "opus".into()],
    };
    ghostcam::quic::send_hello(&mut control_send, &hello).await?;
    info!("sent device hello");

    // Stream enable flags — toggled by bridge commands
    let (video_enabled_tx, video_enabled_rx) = watch::channel(true);
    let (audio_enabled_tx, audio_enabled_rx) = watch::channel(!args.no_audio);
    let (telemetry_enabled_tx, telemetry_enabled_rx) = watch::channel(!args.no_telemetry);

    // Spawn command reader task
    let cmd_device_id = args.device_id.clone();
    tokio::spawn(async move {
        handle_commands(
            control_recv,
            &cmd_device_id,
            video_enabled_tx,
            audio_enabled_tx,
            telemetry_enabled_tx,
        )
        .await;
    });

    // Send loop
    let mut video_ts: u64 = 0;
    let mut audio_ts: u64 = 0;
    let timestamp_step = 1_000_000u64 / args.fps as u64;
    let open_uni_timeout = Duration::from_secs(5);

    loop {
        let msg = match capture_rx.recv().await {
            Some(msg) => msg,
            None => anyhow::bail!("capture channel closed"),
        };

        match msg {
            CaptureMessage::VideoNal { nal_data, nal_type } => {
                if !*video_enabled_rx.borrow() {
                    continue;
                }
                let uni = tokio::time::timeout(open_uni_timeout, connection.open_uni())
                    .await
                    .map_err(|_| anyhow::anyhow!("open_uni timeout"))??;
                stream::send_video_frame(uni, video_ts, &nal_data).await?;

                // Advance timestamp only for VCL NALs
                if nal_type == 1 || nal_type == 5 {
                    video_ts += timestamp_step;
                }
            }
            CaptureMessage::Audio { opus_data } => {
                if !*audio_enabled_rx.borrow() {
                    continue;
                }
                let uni = tokio::time::timeout(open_uni_timeout, connection.open_uni())
                    .await
                    .map_err(|_| anyhow::anyhow!("open_uni timeout"))??;
                stream::send_audio_frame(uni, audio_ts, &opus_data).await?;
                audio_ts += 20_000; // 20ms per Opus frame
            }
            CaptureMessage::Telemetry { msgpack_data } => {
                if !*telemetry_enabled_rx.borrow() {
                    continue;
                }
                let uni = tokio::time::timeout(open_uni_timeout, connection.open_uni())
                    .await
                    .map_err(|_| anyhow::anyhow!("open_uni timeout"))??;
                let ts = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_micros() as u64;
                stream::send_telemetry_frame(uni, ts, &msgpack_data).await?;
            }
        }
    }
}

/// Read and handle commands from the bridge on the QUIC control stream.
async fn handle_commands(
    mut recv: quinn::RecvStream,
    device_id: &str,
    video_tx: watch::Sender<bool>,
    audio_tx: watch::Sender<bool>,
    telemetry_tx: watch::Sender<bool>,
) {
    loop {
        match ghostcam::quic::recv_command(&mut recv).await {
            Ok(cmd) => {
                info!(device_id = %device_id, command = ?cmd, "received command from bridge");
                match cmd {
                    CameraCommand::StartVideo => {
                        let _ = video_tx.send(true);
                    }
                    CameraCommand::StopVideo => {
                        let _ = video_tx.send(false);
                    }
                    CameraCommand::StartAudio => {
                        let _ = audio_tx.send(true);
                    }
                    CameraCommand::StopAudio => {
                        let _ = audio_tx.send(false);
                    }
                    CameraCommand::StartTelemetry => {
                        let _ = telemetry_tx.send(true);
                    }
                    CameraCommand::StopTelemetry => {
                        let _ = telemetry_tx.send(false);
                    }
                    CameraCommand::Configure { .. } => {
                        warn!(device_id = %device_id, "configure command not yet implemented");
                    }
                    CameraCommand::ForceKeyframe => {
                        warn!(device_id = %device_id, "force keyframe not yet implemented");
                    }
                    CameraCommand::ReassignGroup { group_id } => {
                        info!(
                            device_id = %device_id,
                            new_group = %group_id,
                            "group reassignment received (server handles routing)"
                        );
                    }
                    CameraCommand::Custom { name, .. } => {
                        warn!(
                            device_id = %device_id,
                            name = %name,
                            "unknown custom command, ignoring"
                        );
                    }
                }
            }
            Err(e) => {
                info!(device_id = %device_id, error = %e, "command stream ended");
                break;
            }
        }
    }
}

/// Start test source producers: loop H.264 file + Opus silence.
fn start_test_sources(args: &Args, tx: mpsc::Sender<CaptureMessage>) -> Result<()> {
    let nal_units = h264::parse_h264_file(&args.test_file)?;
    info!(nal_count = nal_units.len(), "parsed H.264 test file");

    let fps = args.fps;

    // Video NAL loop
    let tx_video = tx.clone();
    tokio::spawn(async move {
        let frame_interval = Duration::from_micros(1_000_000 / fps as u64);
        loop {
            for nal in &nal_units {
                let nal_type = if nal.is_empty() { 0 } else { nal[0] & 0x1F };
                if tx_video
                    .send(CaptureMessage::VideoNal {
                        nal_data: nal.clone(),
                        nal_type,
                    })
                    .await
                    .is_err()
                {
                    return;
                }
                // Sleep only for VCL NALs
                if nal_type == 1 || nal_type == 5 {
                    tokio::time::sleep(frame_interval).await;
                }
            }
            info!("looped test file, continuing");
        }
    });

    // Audio silence loop
    let tx_audio = tx;
    tokio::spawn(async move {
        const OPUS_FRAME_DURATION: Duration = Duration::from_millis(20);
        loop {
            if tx_audio
                .send(CaptureMessage::Audio {
                    opus_data: Bytes::from_static(stream::OPUS_SILENCE),
                })
                .await
                .is_err()
            {
                return;
            }
            tokio::time::sleep(OPUS_FRAME_DURATION).await;
        }
    });

    // No telemetry in test source mode
    Ok(())
}

/// Start real capture sources: rpicam-vid, cpal audio, system telemetry.
async fn start_real_sources(args: &Args, tx: mpsc::Sender<CaptureMessage>) -> Result<()> {
    // Video capture
    let video_config = capture::video::VideoCaptureConfig {
        width: args.width,
        height: args.height,
        fps: args.fps,
        bitrate: args.bitrate,
        keyframe_interval: args.keyframe_interval,
    };
    let _video = capture::video::VideoCapture::start(video_config, tx.clone()).await?;
    // Leak the handle to keep it alive for the lifetime of the process.
    // The child process will be killed on Drop if we ever add proper shutdown.
    std::mem::forget(_video);

    // Audio capture
    if !args.no_audio {
        match capture::audio::start(tx.clone()) {
            Ok(()) => info!("audio capture started"),
            Err(e) => warn!(error = %e, "audio capture unavailable, continuing without audio"),
        }
    }

    // Telemetry capture
    if !args.no_telemetry {
        let telemetry_config = capture::telemetry::TelemetryCaptureConfig {
            enable_gps: args.enable_gps,
            ..Default::default()
        };
        capture::telemetry::start(telemetry_config, tx);
        info!("telemetry capture started");
    }

    Ok(())
}

/// Drain capture messages during backoff delay (discard frames while disconnected).
async fn drain_during_delay(rx: &mut mpsc::Receiver<CaptureMessage>, delay: Duration) {
    let deadline = tokio::time::Instant::now() + delay;
    loop {
        tokio::select! {
            _ = tokio::time::sleep_until(deadline) => break,
            msg = rx.recv() => {
                if msg.is_none() {
                    break; // channel closed
                }
                // Discard
            }
        }
    }
}
