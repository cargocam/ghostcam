use std::sync::Arc;

use axum::middleware;
use axum::routing::{delete, get, patch, post};
use axum::Router;

use super::auth::auth_middleware;
use super::state::AppState;
use super::{auth, cameras, health, hls, sse, tokens, watch};
use crate::redis::telemetry_api;

pub fn build_router(state: Arc<AppState>) -> Router {
    let protected = Router::new()
        // Cameras
        .route("/api/v1/cameras", get(cameras::list).post(cameras::enroll))
        .route(
            "/api/v1/cameras/:device_id",
            get(cameras::get)
                .patch(cameras::update)
                .delete(cameras::delete),
        )
        // Watch / session
        .route("/api/v1/watch", post(watch::create_session))
        .route(
            "/api/v1/session/:id",
            delete(watch::teardown_session),
        )
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
        .route("/hls/:device_id/init.mp4", get(hls::get_init))
        .route(
            "/hls/:device_id/playlist.m3u8",
            get(hls::get_manifest),
        )
        .route("/hls/:device_id/:segment_id", get(hls::get_segment))
        // Auth (protected - logout, change password)
        .route("/api/v1/auth/logout", post(auth::logout))
        .route("/api/v1/auth/password", patch(auth::change_password))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            auth_middleware,
        ));

    let public = Router::new()
        .route("/healthz", get(health::healthz))
        .route("/readyz", get(health::readyz))
        .route("/api/v1/auth/login", post(auth::login));

    Router::new()
        .merge(protected)
        .merge(public)
        .with_state(state)
}
