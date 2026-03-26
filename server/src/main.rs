mod api;
mod audit;
mod auth;
mod db;
mod db_trait;
mod egress;
mod frames;
mod ingest;
mod pki;
mod redis;
mod sse;

use anyhow::Context;
use std::net::SocketAddr;
use std::sync::Arc;

use crate::api::routes::build_router;
use crate::api::state::AppState;
use crate::db::PostgresDatabase;
use crate::db_trait::Database;
use crate::egress::sessions::SessionManager;
use crate::egress::udp::SharedWebRtcSocket;
use crate::ingest::accept::run_accept_loop;
use crate::ingest::quic_config::build_server_endpoint;
use crate::ingest::registry::RoutingRegistry;
use crate::pki::bootstrap::bootstrap_pki;
use crate::pki::revocation::RevocationCache;
use crate::redis::connection::RedisManager;
use crate::redis::telemetry::TelemetryBatcher;
use crate::sse::SseEventBus;
use tokio_util::sync::CancellationToken;
use tower_http::cors::CorsLayer;
use tower_http::trace::TraceLayer;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();

    // --- Configuration from env ---
    let data_dir =
        std::env::var("GHOSTCAM_DATA_DIR").unwrap_or_else(|_| "/var/ghostcam".to_string());
    std::fs::create_dir_all(&data_dir)?;

    let http_port: u16 = std::env::var("GHOSTCAM_HTTP_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(ghostcam::config::HTTP_PORT);
    let quic_port: u16 = std::env::var("GHOSTCAM_QUIC_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(ghostcam::config::QUIC_PORT);
    let redis_url = std::env::var("GHOSTCAM_REDIS_URL")
        .ok()
        .filter(|s| !s.is_empty());
    let public_ip_override = parse_public_ip_env();
    if let Some(ip) = public_ip_override {
        tracing::info!(ip = %ip, "explicit public IP override");
    } else {
        tracing::info!(
            "no GHOSTCAM_PUBLIC_IP set; ICE candidate IP will be derived from HTTP Host header"
        );
    }
    // enrollment_addr is embedded in enrollment JWTs. Defaults to
    // <public_ip_override>:<quic_port> but can be overridden when cameras and
    // viewers use different network paths (e.g. Docker: cameras reach the
    // server via service DNS, not the LAN IP).
    let enrollment_addr = std::env::var("GHOSTCAM_ENROLLMENT_ADDR")
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| {
            let ip =
                public_ip_override.unwrap_or(std::net::IpAddr::V4(std::net::Ipv4Addr::LOCALHOST));
            format!("{ip}:{quic_port}")
        });

    // --- Database ---
    let database_url =
        std::env::var("GHOSTCAM_DATABASE_URL").context("GHOSTCAM_DATABASE_URL is required")?;
    let db = PostgresDatabase::connect(&database_url).await?;
    let preset_password = std::env::var("GHOSTCAM_ADMIN_PASSWORD")
        .ok()
        .filter(|s| !s.is_empty());
    let admin_email =
        std::env::var("GHOSTCAM_ADMIN_EMAIL").unwrap_or_else(|_| "admin@localhost".to_string());
    if let Some(initial_password) = db
        .initialize(preset_password.as_deref(), &admin_email)
        .await?
    {
        println!("============================================================");
        println!("Ghostcam server first run");
        println!();
        println!("Admin email: {admin_email}");
        println!("Initial admin password: {initial_password}");
        println!();
        if preset_password.is_none() {
            println!("Log in and change this password.");
            println!();
        }
        println!("IMPORTANT: Back up {data_dir}/ca.key");
        println!("Losing this file requires re-enrolling all cameras.");
        println!("============================================================");
    }
    let db: Arc<dyn Database> = Arc::new(db);
    tracing::info!("database connected");

    // --- PKI ---
    let pki = bootstrap_pki(std::path::Path::new(&data_dir)).await?;
    let ca = Arc::new(pki.ca);
    tracing::info!(fingerprint = %pki.server_tls.fingerprint, "PKI ready");

    // --- Revocation cache (must be created before Redis so refresh task can use it) ---
    let revocation_cache = Arc::new(RevocationCache::new());

    // --- Redis (optional) ---
    let cancel = CancellationToken::new();
    let (redis, telemetry_batcher) = if let Some(url) = &redis_url {
        let mgr = RedisManager::new(url, cancel.clone()).await;
        let batcher = Arc::new(TelemetryBatcher::spawn(mgr.clone(), cancel.clone()));
        crate::redis::purge::spawn_telemetry_purge(mgr.clone(), cancel.clone());
        crate::redis::revocation::spawn_revocation_refresh(
            mgr.clone(),
            revocation_cache.clone(),
            cancel.clone(),
        );
        (Some(mgr), Some(batcher))
    } else {
        tracing::info!("redis not configured, telemetry history disabled");
        (None, None)
    };

    // --- Shared WebRTC UDP socket ---
    let webrtc_port: u16 = std::env::var("GHOSTCAM_WEBRTC_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(3478);
    let webrtc_socket = SharedWebRtcSocket::bind(webrtc_port).await?;
    webrtc_socket.clone().spawn_dispatch();
    tracing::info!(port = webrtc_port, "WebRTC UDP listening");

    // --- Audit logger ---
    let audit_hmac_key =
        std::env::var("GHOSTCAM_HMAC_KEY").unwrap_or_else(|_| "dev-hmac-key".to_string());
    let audit_log_path = std::path::PathBuf::from(&data_dir).join("audit.jsonl");
    let audit = Arc::new(crate::audit::AuditLogger::start_with_db(
        &audit_hmac_key,
        audit_log_path,
        db.clone(),
    ));
    audit.log(crate::audit::AuditEvent::ServerStarted {
        version: env!("CARGO_PKG_VERSION").to_string(),
    });
    tracing::info!("audit logger started");

    // --- Shared state ---
    let hmac_secret = db.get_hmac_secret().await?;
    let registry = Arc::new(RoutingRegistry::new());
    let sessions = Arc::new(SessionManager::new());
    let sse_bus = Arc::new(SseEventBus::new());

    let app_state = Arc::new(AppState {
        db: db.clone(),
        redis: redis.clone(),
        registry: registry.clone(),
        sessions: sessions.clone(),
        sse_bus: sse_bus.clone(),
        ca: ca.clone(),
        revocation_cache: revocation_cache.clone(),
        hmac_secret,
        audit: audit.clone(),
        public_ip_override,
        enrollment_addr,
        webrtc_socket,
    });

    // --- QUIC listener ---
    let quic_bind: SocketAddr = format!("0.0.0.0:{quic_port}").parse()?;
    let endpoint =
        build_server_endpoint(&pki.server_tls.cert_der, &pki.server_tls.key_der, quic_bind)?;
    tracing::info!(%quic_bind, "QUIC listening");

    let quic_cancel = cancel.clone();
    let quic_handle = tokio::spawn(run_accept_loop(
        endpoint,
        registry.clone(),
        db.clone(),
        ca.clone(),
        revocation_cache.clone(),
        redis.clone(),
        telemetry_batcher,
        sse_bus.clone(),
        audit.clone(),
        quic_cancel,
    ));

    // --- HTTP server ---
    let router = build_router(app_state)
        .layer(TraceLayer::new_for_http())
        .layer(CorsLayer::permissive());

    let http_bind: SocketAddr = format!("0.0.0.0:{http_port}").parse()?;
    let listener = tokio::net::TcpListener::bind(http_bind).await?;
    tracing::info!(%http_bind, "HTTP listening");

    let http_cancel = cancel.clone();
    let http_handle = tokio::spawn(async move {
        axum::serve(
            listener,
            router.into_make_service_with_connect_info::<std::net::SocketAddr>(),
        )
        .with_graceful_shutdown(async move { http_cancel.cancelled().await })
        .await
    });

    // --- Shutdown ---
    tokio::signal::ctrl_c().await?;
    tracing::info!("shutting down");
    cancel.cancel();

    let _ = tokio::join!(quic_handle, http_handle);
    tracing::info!("goodbye");
    Ok(())
}

/// Parse the public IP from environment variables, if available.
///
/// Priority:
/// 1. `GHOSTCAM_PUBLIC_IP` — explicit override, always wins.
/// 2. `FLY_PUBLIC_IP` — automatically set by Fly.io on each machine.
///
/// Returns `None` when neither is set, letting the watch handler derive the
/// ICE candidate IP from the HTTP request's `Host` header instead.
fn parse_public_ip_env() -> Option<std::net::IpAddr> {
    for var in ["GHOSTCAM_PUBLIC_IP", "FLY_PUBLIC_IP"] {
        if let Some(ip) = std::env::var(var)
            .ok()
            .filter(|s| !s.is_empty())
            .and_then(|s| s.parse::<std::net::IpAddr>().ok())
        {
            return Some(ip);
        }
    }
    None
}
