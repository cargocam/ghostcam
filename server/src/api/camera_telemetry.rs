use std::sync::Arc;

use axum::extract::State;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::api_types::{CameraCommand, TelemetryPollRequest, TelemetryPollResponse};

use super::auth::AuthCamera;
use super::state::AppState;
use crate::redis::telemetry::write_telemetry_batch;

/// POST /api/v1/cameras/:id/telemetry
///
/// Camera-auth endpoint. Camera posts telemetry every 10s.
/// Response contains any pending commands from the server.
pub async fn post_telemetry(
    State(state): State<Arc<AppState>>,
    Extension(camera): Extension<AuthCamera>,
    Json(body): Json<TelemetryPollRequest>,
) -> Response {
    let device_id = &camera.device_id;

    // Write telemetry to Redis
    if let Some(ref redis) = state.redis {
        write_telemetry_batch(redis, device_id, &[body.telemetry]).await;
    }

    // Claim pending commands
    let commands = match state.db.claim_commands(device_id).await {
        Ok(cmd_values) => cmd_values
            .into_iter()
            .filter_map(|v| serde_json::from_value::<CameraCommand>(v).ok())
            .collect(),
        Err(e) => {
            tracing::warn!(device_id = %device_id, "failed to claim commands: {e}");
            Vec::new()
        }
    };

    Json(TelemetryPollResponse { commands }).into_response()
}
