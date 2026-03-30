use std::sync::Arc;

use axum::extract::DefaultBodyLimit;
use axum::middleware;
use axum::routing::{delete, get, patch, post};
use axum::Router;
use ghostcam::config::MAX_REQUEST_BODY_BYTES;
use tower_http::services::{ServeDir, ServeFile};

use super::auth::auth_middleware;
use super::rate_limit::{api_rate_limit, login_rate_limit, ApiRateLimiter, LoginRateLimiter};
use super::state::AppState;
use super::{
    admin, audit, auth, billing, cameras, firmware, github_webhook, health, hls, qr, sse, tokens,
    watch,
};
use crate::redis::telemetry_api;

pub fn build_router(state: Arc<AppState>) -> Router {
    let api_limiter = ApiRateLimiter::new();
    let login_limiter = LoginRateLimiter::new();

    let protected = Router::new()
        // Cameras
        .route("/api/v1/cameras", get(cameras::list).post(cameras::enroll))
        .route("/api/v1/cameras/unclaimed", get(cameras::list_unclaimed))
        .route(
            "/api/v1/cameras/enroll/qr",
            get(qr::enrollment_qr).post(qr::enrollment_qr),
        )
        .route(
            "/api/v1/cameras/:device_id",
            get(cameras::get)
                .patch(cameras::update)
                .delete(cameras::delete),
        )
        // Watch / session
        .route("/api/v1/watch", post(watch::create_session))
        .route("/api/v1/session/:id", delete(watch::teardown_session))
        .route("/api/v1/session/:id/ice", post(watch::ice_candidate))
        // API tokens
        .route("/api/v1/tokens", get(tokens::list).post(tokens::create))
        .route("/api/v1/tokens/:token_id", delete(tokens::revoke))
        // Telemetry
        .route(
            "/api/v1/telemetry/:device_id/latest",
            get(telemetry_api::handle_latest),
        )
        .route(
            "/api/v1/telemetry/:device_id",
            get(telemetry_api::handle_range),
        )
        // SSE
        .route("/events", get(sse::handle_sse))
        // HLS
        .route("/hls/:device_id/coverage", get(hls::get_coverage))
        .route("/hls/:device_id/init.mp4", get(hls::get_init))
        .route("/hls/:device_id/playlist.m3u8", get(hls::get_manifest))
        .route("/hls/:device_id/prefetch", post(hls::prefetch))
        .route("/hls/:device_id/:segment_id", get(hls::get_segment))
        // Billing
        .route(
            "/api/v1/billing/subscription",
            get(billing::get_subscription),
        )
        .route("/api/v1/billing/portal", post(billing::create_portal))
        .route("/api/v1/billing/usage", get(billing::get_usage))
        // Audit
        .route("/api/v1/audit", get(audit::query))
        // Admin
        .route("/api/v1/admin/reload", post(admin::reload_config))
        // Auth (protected - logout, change password)
        .route("/api/v1/auth/logout", post(auth::logout))
        .route("/api/v1/auth/password", patch(auth::change_password))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            auth_middleware,
        ))
        .layer(middleware::from_fn_with_state(api_limiter, api_rate_limit));

    // Login endpoint with its own stricter rate limit (5 req/min per IP).
    let login = Router::new()
        .route("/api/v1/auth/login", post(auth::login))
        .layer(middleware::from_fn_with_state(
            login_limiter,
            login_rate_limit,
        ));

    let public = Router::new()
        .route("/healthz", get(health::healthz))
        .route("/readyz", get(health::readyz))
        .merge(login)
        .route("/api/v1/auth/register", post(auth::register))
        .route("/api/v1/billing/tiers", get(billing::list_tiers))
        .route("/api/v1/webhooks/stripe", post(billing::stripe_webhook))
        .route("/api/v1/firmware/latest", get(firmware::get_latest))
        .route(
            "/api/v1/webhooks/github",
            post(github_webhook::github_webhook),
        );

    // Serve the SPA static files in production. Falls back to index.html
    // for client-side routing. Only active if /app/static exists (Docker build).
    let static_dir = std::path::Path::new("/app/static");
    let spa_fallback = if static_dir.exists() {
        let serve = ServeDir::new(static_dir)
            .not_found_service(ServeFile::new(static_dir.join("index.html")));
        Some(serve)
    } else {
        None
    };

    let mut router = Router::new()
        .merge(protected)
        .merge(public)
        .layer(DefaultBodyLimit::max(MAX_REQUEST_BODY_BYTES))
        .with_state(state);

    if let Some(spa) = spa_fallback {
        router = router.fallback_service(spa);
    }

    router
}
