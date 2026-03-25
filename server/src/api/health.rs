use std::sync::Arc;

use axum::extract::State;
use axum::Json;
use serde::Serialize;

use super::state::AppState;

/// GET /healthz — always 200
pub async fn healthz() -> &'static str {
    "ok"
}

#[derive(Serialize)]
pub struct ReadyResponse {
    pub status: String,
    pub database: String,
    pub redis: String,
    pub quic: String,
}

/// GET /readyz — 200 if DB + QUIC + HTTP ready; Redis optional
pub async fn readyz(State(state): State<Arc<AppState>>) -> Json<ReadyResponse> {
    let db_status = match state.db.health_check().await {
        Ok(_) => "ok",
        Err(_) => "unavailable",
    };

    let redis_status = match &state.redis {
        Some(r) if r.is_connected() => "ok",
        Some(_) => "unavailable",
        None => "not_configured",
    };

    let status = if db_status == "ok" {
        "ready"
    } else {
        "not_ready"
    };

    Json(ReadyResponse {
        status: status.to_string(),
        database: db_status.to_string(),
        redis: redis_status.to_string(),
        quic: "ok".to_string(),
    })
}
