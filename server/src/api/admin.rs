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

/// `POST /api/v1/admin/reload` — reload server configuration from disk.
///
/// Re-reads the config file and publishes the new config via the watch channel.
/// Only settings that don't require socket/pool rebinding take effect immediately.
/// Settings that require restart (database_url, ports) are logged as warnings.
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

/// Perform the actual config reload, returning a summary message.
pub fn do_reload(state: &AppState) -> anyhow::Result<String> {
    let new_config = ServerConfig::load()?;
    let mut warnings = Vec::new();

    if let Some(ref tx) = state.config_tx {
        let old = tx.borrow().clone();

        // Check for non-reloadable changes
        if old.http_port != new_config.http_port {
            warnings.push(format!(
                "http_port changed ({} -> {}), requires restart",
                old.http_port, new_config.http_port
            ));
        }
        if old.quic_port != new_config.quic_port {
            warnings.push(format!(
                "quic_port changed ({} -> {}), requires restart",
                old.quic_port, new_config.quic_port
            ));
        }
        if old.webrtc_port != new_config.webrtc_port {
            warnings.push(format!(
                "webrtc_port changed ({} -> {}), requires restart",
                old.webrtc_port, new_config.webrtc_port
            ));
        }
        if old.database_url != new_config.database_url {
            warnings.push("database_url changed, requires restart".to_string());
        }
        if old.data_dir != new_config.data_dir {
            warnings.push(format!(
                "data_dir changed ({} -> {}), requires restart",
                old.data_dir, new_config.data_dir
            ));
        }

        for w in &warnings {
            tracing::warn!("config reload: {w}");
        }

        tx.send(new_config)?;
        tracing::info!("configuration reloaded");
    } else {
        return Ok("no config watch channel configured".to_string());
    }

    if warnings.is_empty() {
        Ok("configuration reloaded successfully".to_string())
    } else {
        Ok(format!(
            "configuration reloaded with warnings: {}",
            warnings.join("; ")
        ))
    }
}
