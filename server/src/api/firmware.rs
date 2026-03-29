use std::sync::Arc;

use axum::extract::State;
use axum::response::{IntoResponse, Response};
use axum::Json;
use ghostcam::firmware::FirmwareLatestResponse;

use super::state::AppState;

/// GET /api/v1/firmware/latest
///
/// Public endpoint (no auth) — cameras call this before completing handshake.
/// Returns the latest known firmware release, or `{"version": null}` if unknown.
pub async fn get_latest(State(state): State<Arc<AppState>>) -> Response {
    let release = state.firmware_release.read().await;
    match release.as_ref() {
        Some(fw) => Json(FirmwareLatestResponse {
            version: Some(fw.version.clone()),
            assets: Some(fw.assets.clone()),
        })
        .into_response(),
        None => Json(FirmwareLatestResponse {
            version: None,
            assets: None,
        })
        .into_response(),
    }
}
