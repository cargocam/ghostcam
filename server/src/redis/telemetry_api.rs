use std::sync::Arc;

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::Extension;
use axum::Json;
use ghostcam::types::DeviceId;
use serde::Deserialize;

use super::telemetry::TelemetryEntry;
use super::telemetry_query::{self, TelemetryPage};
use crate::api::auth::AuthUser;
use crate::api::state::AppState;

/// Query parameters for telemetry range requests.
#[derive(Deserialize)]
pub struct TelemetryRangeParams {
    pub from: u64,
    pub to: u64,
    pub cursor: Option<String>,
    pub limit: Option<usize>,
}

/// Verify the authenticated user owns the given camera.
/// Returns 404 for both non-existent and not-owned cameras to avoid
/// leaking camera existence to other users.
async fn verify_ownership(
    state: &AppState,
    user: &AuthUser,
    device_id: &DeviceId,
) -> Result<(), StatusCode> {
    match state.db.get_camera(device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => Ok(()),
        Ok(Some(_)) | Ok(None) => Err(StatusCode::NOT_FOUND),
        Err(_) => Err(StatusCode::INTERNAL_SERVER_ERROR),
    }
}

/// GET /telemetry/{device_id}/latest
pub async fn handle_latest(
    Path(device_id): Path<String>,
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Result<Json<TelemetryEntry>, StatusCode> {
    let device_id = DeviceId(device_id);

    verify_ownership(&state, &user, &device_id).await?;

    let redis = state.redis.as_ref().ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
    if !redis.is_connected() {
        return Err(StatusCode::SERVICE_UNAVAILABLE);
    }

    match telemetry_query::get_latest(redis, &device_id).await {
        Ok(Some(entry)) => Ok(Json(entry)),
        Ok(None) => Err(StatusCode::NOT_FOUND),
        Err(_) => Err(StatusCode::INTERNAL_SERVER_ERROR),
    }
}

/// GET /telemetry/{device_id}?from={}&to={}&cursor={}&limit={}
pub async fn handle_range(
    Path(device_id): Path<String>,
    Query(params): Query<TelemetryRangeParams>,
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Result<Json<TelemetryPage>, StatusCode> {
    let device_id = DeviceId(device_id);

    verify_ownership(&state, &user, &device_id).await?;

    let redis = state.redis.as_ref().ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
    if !redis.is_connected() {
        return Err(StatusCode::SERVICE_UNAVAILABLE);
    }

    match telemetry_query::query_range(
        redis,
        &device_id,
        params.from,
        params.to,
        params.cursor.as_deref(),
        params.limit,
    )
    .await
    {
        Ok(page) => Ok(Json(page)),
        Err(_) => Err(StatusCode::INTERNAL_SERVER_ERROR),
    }
}
