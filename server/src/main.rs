mod api;
mod audit;
mod auth;
mod billing;
mod config;
mod db;
mod db_trait;
mod firmware;
mod redis;
mod s3;

use std::net::SocketAddr;
use std::sync::Arc;

use crate::api::routes::build_router;
use crate::api::state::AppState;
use crate::config::ServerConfig;
use crate::db::PostgresDatabase;
use crate::db_trait::Database;
use crate::redis::connection::RedisManager;
use crate::redis::telemetry::TelemetryBatcher;
use tokio_util::sync::CancellationToken;
use tower_http::cors::CorsLayer;
use tower_http::trace::TraceLayer;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();

    // --- Configuration ---
    let cfg = ServerConfig::load()?;
    std::fs::create_dir_all(&cfg.data_dir)?;

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
        println!("============================================================");
    }
    let db: Arc<dyn Database> = Arc::new(db);
    tracing::info!("database connected");

    // --- Redis (optional) ---
    let cancel = CancellationToken::new();
    let (redis, _telemetry_batcher) = if let Some(url) = &cfg.redis_url {
        let mgr = RedisManager::new(url, cancel.clone()).await;
        let batcher = Arc::new(TelemetryBatcher::spawn(mgr.clone(), cancel.clone()));
        crate::redis::purge::spawn_telemetry_purge(mgr.clone(), cancel.clone());
        (Some(mgr), Some(batcher))
    } else {
        tracing::info!("redis not configured, telemetry history disabled");
        (None, None)
    };

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
    let stripe = if let Some(key) = &cfg.stripe_secret_key {
        let webhook_secret = cfg.stripe_webhook_secret.as_deref();
        if webhook_secret.is_none() {
            tracing::warn!("STRIPE_WEBHOOK_SECRET not set — webhooks will be rejected");
        }
        let client = billing::stripe_client::StripeClient::new(key, webhook_secret);
        tracing::info!("stripe billing enabled");
        Some(Arc::new(client))
    } else {
        tracing::info!("stripe not configured, billing disabled (unlimited free tier)");
        None
    };

    if stripe.is_some() {
        billing::background::spawn_grace_period_check(db.clone(), audit.clone(), cancel.clone());
    }

    // --- S3 / Tigris ---
    let s3 = match crate::s3::S3Client::new(&cfg).await {
        Ok(client) => {
            tracing::info!(bucket = %cfg.s3_bucket, "S3/Tigris client initialized");
            Some(Arc::new(client))
        }
        Err(e) => {
            tracing::warn!("S3 client init failed (segment uploads disabled): {e}");
            None
        }
    };

    // --- Shared state ---
    let hmac_secret = db.get_hmac_secret().await?;

    // --- Firmware release state ---
    let firmware_release = Arc::new(tokio::sync::RwLock::new(None));

    if let Some(ref repo) = cfg.release_repo {
        let fw = firmware_release.clone();
        let repo = repo.clone();
        tokio::spawn(async move {
            tracing::info!(repo = %repo, "fetching latest release from GitHub API");
            match crate::api::github_webhook::fetch_latest_github_release(&repo).await {
                Some(release) => {
                    tracing::info!(version = %release.version, "seeded firmware state from GitHub");
                    *fw.write().await = Some(release);
                }
                None => {
                    tracing::info!("no firmware release found on GitHub (or fetch failed)");
                }
            }
        });
    }

    let app_state = Arc::new(AppState {
        db: db.clone(),
        redis: redis.clone(),
        hmac_secret,
        audit: audit.clone(),
        s3,
        stripe,
        tiers,
        stripe_public_key: cfg.stripe_public_key.clone(),
        stripe_pricing_table_id: cfg.stripe_pricing_table_id.clone(),
        stripe_portal_config_id: cfg.stripe_portal_config_id.clone(),
        firmware_release: firmware_release.clone(),
        github_webhook_secret: cfg.github_webhook_secret.clone(),
        update_stagger_secs: cfg.update_stagger_secs,
        pending_reboot_version: tokio::sync::Mutex::new(None),
    });

    // --- Hourly cleanup of expired sessions ---
    {
        let cleanup_db = db.clone();
        let cleanup_cancel = cancel.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(3600));
            interval.tick().await;
            loop {
                tokio::select! {
                    _ = cleanup_cancel.cancelled() => break,
                    _ = interval.tick() => {
                        match cleanup_db.cleanup_expired_sessions().await {
                            Ok(n) if n > 0 => tracing::info!(count = n, "cleaned up expired sessions"),
                            Err(e) => tracing::warn!("session cleanup failed: {e}"),
                            _ => {}
                        }
                    }
                }
            }
        });
    }

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

    let _ = http_handle.await;
    tracing::info!("goodbye");
    Ok(())
}
