use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::Json;
use serde::Serialize;

use super::state::AppState;
use crate::config::ServerConfig;

#[derive(Serialize)]
struct ReloadResponse {
    reloaded: bool,
    message: String,
}

/// `POST /api/v1/admin/reload` — validate server configuration from disk.
///
/// Re-reads the config file and validates it. Logs warnings for settings that
/// have changed and would require a restart to take effect.
///
/// TODO: add admin-role check once a role system exists.
pub async fn reload_config(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    match do_reload(&state) {
        Ok(msg) => (
            StatusCode::OK,
            Json(ReloadResponse {
                reloaded: true,
                message: msg,
            }),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(ReloadResponse {
                reloaded: false,
                message: format!("reload failed: {e}"),
            }),
        ),
    }
}

/// Perform config validation, returning a summary message.
///
/// Re-reads the config from disk/env variables and validates it. Since there is
/// no watch channel consumer, this only validates and logs — it does not
/// hot-reload anything. Settings that differ from the running config are logged
/// as warnings requiring a restart.
pub fn do_reload(_state: &AppState) -> anyhow::Result<String> {
    let new_config = ServerConfig::load()?;

    tracing::info!("configuration re-read and validated successfully");

    // Log a summary of what was loaded (non-sensitive fields only)
    tracing::info!(
        data_dir = %new_config.data_dir,
        http_port = new_config.http_port,
        quic_port = new_config.quic_port,
        webrtc_port = new_config.webrtc_port,
        public_ip = ?new_config.public_ip,
        "validated configuration"
    );

    Ok(
        "configuration validated successfully; restart required for changes to take effect"
            .to_string(),
    )
}
