mod quic;
mod stream;
mod test_source;

use clap::Parser;
use ghostcam_common::{group::GroupId, hello::DeviceHello};
use std::path::PathBuf;
use tracing::info;

#[derive(Parser)]
#[command(name = "ghostcam-agent", about = "Ghostcam test camera agent")]
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

    /// Path to raw H.264 annex-B test file
    #[arg(long, default_value = "test-data/test.h264")]
    test_file: PathBuf,

    /// Target frames per second
    #[arg(long, default_value = "30")]
    fps: u32,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "ghostcam_agent=info".into()),
        )
        .init();

    let args = Args::parse();

    info!(
        device_id = %args.device_id,
        group_id = %args.group_id,
        bridge = %args.bridge_addr,
        fps = args.fps,
        "starting ghostcam agent"
    );

    // Parse NAL units from test file
    let nal_units = test_source::parse_h264_file(&args.test_file)?;
    info!(nal_count = nal_units.len(), "parsed H.264 test file");

    // Connect to bridge
    let (connection, mut control_send, _control_recv) =
        quic::connect(&args.bridge_addr).await?;
    info!("connected to bridge");

    // Send hello on control stream
    let hello = DeviceHello {
        device_id: args.device_id.clone(),
        group_id: GroupId::new(&args.group_id),
        capabilities: vec!["h264".into(), "opus".into()],
    };
    let hello_json = serde_json::to_vec(&hello)?;
    control_send
        .write_all(&(hello_json.len() as u32).to_be_bytes())
        .await?;
    control_send.write_all(&hello_json).await?;
    info!("sent device hello");

    // Spawn audio task — sends Opus silence frames at 20ms intervals
    let audio_conn = connection.clone();
    tokio::spawn(async move {
        let mut audio_ts: u64 = 0;
        const OPUS_FRAME_DURATION_US: u64 = 20_000; // 20ms
        loop {
            match audio_conn.open_uni().await {
                Ok(uni) => {
                    if let Err(e) =
                        stream::send_audio_frame(uni, audio_ts, stream::OPUS_SILENCE).await
                    {
                        tracing::warn!(error = %e, "audio send error");
                        break;
                    }
                }
                Err(e) => {
                    tracing::warn!(error = %e, "audio stream open error");
                    break;
                }
            }
            audio_ts += OPUS_FRAME_DURATION_US;
            tokio::time::sleep(std::time::Duration::from_micros(OPUS_FRAME_DURATION_US)).await;
        }
    });

    // Stream video frames in a loop.
    // NALs are grouped into access units: non-VCL NALs (SPS/PPS/SEI) share
    // the same timestamp as the following VCL NAL (IDR/slice).
    let frame_interval = std::time::Duration::from_micros(1_000_000 / args.fps as u64);
    let mut timestamp_us: u64 = 0;
    let timestamp_step = 1_000_000 / args.fps as u64;

    loop {
        for nal in &nal_units {
            let uni = connection.open_uni().await?;
            stream::send_video_frame(uni, timestamp_us, nal).await?;

            // Only advance timestamp and sleep for VCL NALs (actual video frames).
            // Non-VCL NALs (SPS=7, PPS=8, SEI=6, AUD=9) are part of the same
            // access unit as the next VCL NAL and share its timestamp.
            let nal_type = nal[0] & 0x1F;
            if nal_type == 1 || nal_type == 5 {
                timestamp_us += timestamp_step;
                tokio::time::sleep(frame_interval).await;
            }
        }
        info!(
            timestamp_ms = timestamp_us / 1000,
            "looped test file, continuing"
        );
    }
}
