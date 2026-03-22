use std::net::SocketAddr;
use std::sync::Arc;

use server_core::api::routes::build_router;
use server_core::api::state::AppState;
use server_core::egress::sessions::SessionManager;
use server_core::ingest::accept::run_accept_loop;
use server_core::ingest::quic_config::build_server_endpoint;
use server_core::ingest::registry::RoutingRegistry;
use server_core::pki::bootstrap::bootstrap_pki;
use server_core::pki::revocation::RevocationCache;
use server_core::redis::connection::RedisManager;
use server_core::sse::SseEventBus;
use server_solo::db::SqliteDatabase;
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

    let public_ip = std::env::var("GHOSTCAM_PUBLIC_IP").unwrap_or_else(|_| "127.0.0.1".into());
    let http_port: u16 = std::env::var("GHOSTCAM_HTTP_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(ghostcam::config::HTTP_PORT);
    let quic_port: u16 = std::env::var("GHOSTCAM_QUIC_PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(ghostcam::config::QUIC_PORT);
    let redis_url = std::env::var("GHOSTCAM_REDIS_URL").ok().filter(|s| !s.is_empty());
    let public_addr: SocketAddr = format!("{public_ip}:{quic_port}").parse()?;

    // --- Database ---
    let db_path = format!("{data_dir}/ghostcam.db");
    let db = SqliteDatabase::open(&db_path).await?;
    if let Some(initial_password) = db.initialize().await? {
        println!("============================================================");
        println!("Ghostcam server-solo first run");
        println!();
        println!("Initial operator password: {initial_password}");
        println!();
        println!("Log in and change this password.");
        println!();
        println!("IMPORTANT: Back up {data_dir}/ca.key");
        println!("Losing this file requires re-enrolling all cameras.");
        println!("============================================================");
    }
    let db: Arc<dyn server_core::db::Database> = Arc::new(db);
    tracing::info!("database initialized at {db_path}");

    // --- PKI ---
    let pki = bootstrap_pki(std::path::Path::new(&data_dir)).await?;
    let ca = Arc::new(pki.ca);
    tracing::info!(fingerprint = %pki.server_tls.fingerprint, "PKI ready");

    // --- Redis (optional) ---
    let cancel = CancellationToken::new();
    let redis = if let Some(url) = &redis_url {
        let mgr = Arc::new(RedisManager::new(url).await);
        mgr.spawn_reconnect_loop(cancel.clone());
        tracing::info!("redis connected");
        Some(mgr)
    } else {
        tracing::info!("redis not configured, telemetry history disabled");
        None
    };

    // --- Shared state ---
    let hmac_secret = db.get_hmac_secret().await?;
    let registry = Arc::new(RoutingRegistry::new());
    let sessions = Arc::new(SessionManager::new());
    let sse_bus = Arc::new(SseEventBus::new());
    let revocation_cache = Arc::new(RevocationCache::new());

    let app_state = Arc::new(AppState {
        db: db.clone(),
        redis: redis.clone(),
        registry: registry.clone(),
        sessions: sessions.clone(),
        sse_bus: sse_bus.clone(),
        ca: ca.clone(),
        revocation_cache: revocation_cache.clone(),
        hmac_secret,
        public_addr,
    });

    // --- QUIC listener ---
    let quic_bind: SocketAddr = format!("0.0.0.0:{quic_port}").parse()?;
    let endpoint = build_server_endpoint(
        &pki.server_tls.cert_der,
        &pki.server_tls.key_der,
        quic_bind,
    )?;
    tracing::info!(%quic_bind, "QUIC listening");

    let quic_cancel = cancel.clone();
    let quic_handle = tokio::spawn(run_accept_loop(
        endpoint,
        registry.clone(),
        db.clone(),
        ca.clone(),
        revocation_cache.clone(),
        redis.clone(),
        sse_bus.clone(),
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
        axum::serve(listener, router)
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
