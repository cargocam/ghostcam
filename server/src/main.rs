mod api;
mod audit;
mod auth;
mod billing;
mod config;
mod db;
mod db_trait;
mod egress;
mod frames;
mod ingest;
mod pki;
mod redis;
mod sse;

use std::net::SocketAddr;
use std::sync::Arc;

use crate::api::routes::build_router;
use crate::api::state::AppState;
use crate::config::ServerConfig;
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

    // --- Configuration ---
    let cfg = ServerConfig::load()?;

    std::fs::create_dir_all(&cfg.data_dir)?;

    if let Some(ip) = cfg.public_ip {
        tracing::info!(ip = %ip, "explicit public IP override");
    } else {
        tracing::info!(
            "no GHOSTCAM_PUBLIC_IP set; ICE candidate IP will be derived from HTTP Host header"
        );
    }
    let enrollment_addr = cfg.resolved_enrollment_addr();

    // --- Database ---
    let db = PostgresDatabase::connect(&cfg.database_url).await?;
    if let Some(initial_password) = db
        .initialize(cfg.admin_password.as_deref(), &cfg.admin_email)
        .await?
    {
        println!("============================================================");
        println!("Ghostcam server first run");
        println!();
        println!("Admin email: {}", cfg.admin_email);
        println!("Initial admin password: {initial_password}");
        println!();
        if cfg.admin_password.is_none() {
            println!("Log in and change this password.");
            println!();
        }
        println!("IMPORTANT: Back up {}/ca.key", cfg.data_dir);
        println!("Losing this file requires re-enrolling all cameras.");
        println!("============================================================");
    }
    let db: Arc<dyn Database> = Arc::new(db);
    tracing::info!("database connected");

    // --- PKI ---
    let pki = bootstrap_pki(std::path::Path::new(&cfg.data_dir)).await?;
    let ca = Arc::new(pki.ca);
    tracing::info!(fingerprint = %pki.server_tls.fingerprint, "PKI ready");

    // --- Revocation cache (must be created before Redis so refresh task can use it) ---
    let revocation_cache = Arc::new(RevocationCache::new());

    // --- Redis (optional) ---
    let cancel = CancellationToken::new();
    let (redis, telemetry_batcher) = if let Some(url) = &cfg.redis_url {
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
    let webrtc_socket = SharedWebRtcSocket::bind(cfg.webrtc_port).await?;
    webrtc_socket.clone().spawn_dispatch();
    tracing::info!(port = cfg.webrtc_port, "WebRTC UDP listening");

    // --- Audit logger ---
    let audit_hmac_key =
        std::env::var("GHOSTCAM_HMAC_KEY").unwrap_or_else(|_| "dev-hmac-key".to_string());
    let audit_log_path = std::path::PathBuf::from(&cfg.data_dir).join("audit.jsonl");
    let audit = Arc::new(crate::audit::AuditLogger::start_with_db(
        &audit_hmac_key,
        audit_log_path,
        db.clone(),
    ));
    audit.log(crate::audit::AuditEvent::ServerStarted {
        version: env!("CARGO_PKG_VERSION").to_string(),
    });
    tracing::info!("audit logger started");

    // --- Billing (optional) ---
    let tiers = Arc::new(billing::tiers::TierRegistry::default());
    let stripe = if let (Some(key), Some(webhook_secret)) =
        (&cfg.stripe_secret_key, &cfg.stripe_webhook_secret)
    {
        let mut price_ids = std::collections::HashMap::new();
        if let Some(p) = &cfg.stripe_price_id_starter {
            price_ids.insert("starter".into(), p.clone());
        }
        if let Some(p) = &cfg.stripe_price_id_pro {
            price_ids.insert("pro".into(), p.clone());
        }
        if let Some(p) = &cfg.stripe_price_id_enterprise {
            price_ids.insert("enterprise".into(), p.clone());
        }
        let client = billing::stripe_client::StripeClient::new(key, webhook_secret, price_ids);
        tracing::info!("stripe billing enabled");
        Some(Arc::new(client))
    } else {
        tracing::info!("stripe not configured, billing disabled (unlimited free tier)");
        None
    };

    if stripe.is_some() {
        billing::background::spawn_grace_period_check(db.clone(), audit.clone(), cancel.clone());
    }

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
        public_ip_override: cfg.public_ip,
        enrollment_addr,
        webrtc_socket,
        stripe,
        tiers,
        stripe_portal_config_id: cfg.stripe_portal_config_id,
    });

    // --- QUIC listener ---
    let quic_bind: SocketAddr = format!("0.0.0.0:{}", cfg.quic_port).parse()?;
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
    let router = build_router(app_state.clone())
        .layer(TraceLayer::new_for_http())
        .layer(CorsLayer::permissive());

    let http_bind: SocketAddr = format!("0.0.0.0:{}", cfg.http_port).parse()?;
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

    // --- SIGHUP reload handler ---
    #[cfg(unix)]
    {
        let reload_state = app_state.clone();
        tokio::spawn(async move {
            let mut sighup = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::hangup())
                .expect("failed to register SIGHUP handler");
            loop {
                sighup.recv().await;
                tracing::info!("SIGHUP received, reloading configuration");
                match crate::api::admin::do_reload(&reload_state) {
                    Ok(msg) => tracing::info!("config reload: {msg}"),
                    Err(e) => tracing::error!("config reload failed: {e}"),
                }
            }
        });
    }

    // --- Shutdown ---
    tokio::signal::ctrl_c().await?;
    tracing::info!("shutting down");
    cancel.cancel();

    let _ = tokio::join!(quic_handle, http_handle);
    tracing::info!("goodbye");
    Ok(())
}
