mod api;
mod metrics;
mod quic;
mod webrtc;

use clap::Parser;
use ghostcam::audit::AuditLogger;
use ghostcam::config;
use ghostcam::router::GroupRouter;
use metrics::Metrics;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::RwLock;
use tracing::info;
use webrtc::WebRtcEngine;

#[derive(Parser)]
#[command(name = "server", about = "Ghostcam bridge server")]
struct Args {
    /// QUIC listen port for camera connections
    #[arg(long, default_value_t = config::DEFAULT_QUIC_PORT)]
    quic_port: u16,

    /// HTTP listen port for viewer API
    #[arg(long, default_value_t = config::DEFAULT_HTTP_PORT)]
    http_port: u16,

    /// Public IP for ICE candidates
    #[arg(long, default_value = "127.0.0.1")]
    public_ip: IpAddr,

    /// API key for viewer authentication
    #[arg(long, env = "GHOSTCAM_API_KEY", default_value = "dev-key")]
    api_key: String,

    /// HMAC key for audit log integrity
    #[arg(long, env = "GHOSTCAM_HMAC_KEY", default_value = "dev-hmac-key")]
    hmac_key: String,

    /// Directory to serve built viewer from
    #[arg(long)]
    viewer_dir: Option<PathBuf>,
}

pub struct AppState {
    pub router: RwLock<GroupRouter>,
    pub webrtc_cmd_tx: tokio::sync::mpsc::Sender<webrtc::WebRtcCommand>,
    pub api_key: String,
    pub public_ip: IpAddr,
    pub audit: Arc<AuditLogger>,
    pub metrics: Arc<Metrics>,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "server=info,str0m=warn".into()),
        )
        .init();

    let args = Args::parse();

    info!(
        quic_port = args.quic_port,
        http_port = args.http_port,
        public_ip = %args.public_ip,
        "starting ghostcam bridge"
    );

    let (webrtc_cmd_tx, webrtc_cmd_rx) = tokio::sync::mpsc::channel(256);

    let group_router = GroupRouter::new();
    let frame_tx = group_router.frame_tx.clone();
    let camera_event_rx = group_router.event_tx.subscribe();

    let audit = Arc::new(AuditLogger::new(args.hmac_key.as_bytes()));
    let metrics = Arc::new(Metrics::new());

    let state = Arc::new(AppState {
        router: RwLock::new(group_router),
        webrtc_cmd_tx,
        api_key: args.api_key,
        public_ip: args.public_ip,
        audit,
        metrics,
    });

    // Spawn QUIC listener for cameras
    let quic_state = state.clone();
    let quic_port = args.quic_port;
    tokio::spawn(async move {
        if let Err(e) = quic::run_quic_listener(quic_port, quic_state).await {
            tracing::error!(error = %e, "QUIC listener failed");
        }
    });

    // Spawn WebRTC engine
    let webrtc_state = state.clone();
    let webrtc_frame_rx = frame_tx.subscribe();
    tokio::spawn(async move {
        let mut engine = WebRtcEngine::new(webrtc_state, webrtc_cmd_rx, webrtc_frame_rx, camera_event_rx).await;
        engine.run().await;
    });

    // Run HTTP server (blocking)
    let http_addr = SocketAddr::new(IpAddr::V4(Ipv4Addr::UNSPECIFIED), args.http_port);
    let app = api::create_router(state.clone(), args.viewer_dir);
    info!(addr = %http_addr, "HTTP server listening");
    let listener = tokio::net::TcpListener::bind(http_addr).await?;
    axum::serve(listener, app).await?;

    Ok(())
}
