use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::types::DeviceId;
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use super::auth::AuthUser;
use super::state::AppState;
use crate::egress::handle::EgressHandle;

#[derive(Deserialize)]
pub struct WatchRequest {
    pub sdp_offer: String,
    pub device_id: String,
}

#[derive(Serialize)]
pub struct WatchResponse {
    pub session_id: String,
    pub sdp_answer: String,
}

/// POST /api/v1/watch
pub async fn create_session(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(body): Json<WatchRequest>,
) -> Response {
    let device_id = DeviceId(body.device_id);

    // Verify camera belongs to user
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        Ok(Some(_)) | Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    // Look up slot
    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => {
            return (StatusCode::CONFLICT, "camera is offline").into_response();
        }
    };

    // Create egress handle
    let session_id = uuid::Uuid::new_v4().to_string();
    let cancel = CancellationToken::new();

    let (egress, sdp_answer) = match EgressHandle::create(
        session_id.clone(),
        &slot,
        &body.sdp_offer,
        state.public_addr,
        cancel.clone(),
    )
    .await
    {
        Ok(r) => r,
        Err(e) => {
            tracing::error!("failed to create egress handle: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Spawn the egress event loop
    let handle = tokio::spawn(async move {
        if let Err(e) = egress.run().await {
            tracing::warn!("egress session ended with error: {e}");
        }
    });

    // Register session
    state
        .sessions
        .register(
            session_id.clone(),
            device_id,
            user.user_id,
            cancel,
            handle,
        )
        .await;

    Json(WatchResponse {
        session_id,
        sdp_answer,
    })
    .into_response()
}

/// DELETE /api/v1/session/:id
pub async fn teardown_session(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(session_id): Path<String>,
) -> Response {
    // Verify ownership
    match state.sessions.get_user_id(&session_id).await {
        Some(uid) if uid == user.user_id => {}
        Some(_) => return StatusCode::FORBIDDEN.into_response(),
        None => return StatusCode::NOT_FOUND.into_response(),
    }

    state.sessions.teardown(&session_id).await;
    StatusCode::OK.into_response()
}

/// POST /api/v1/session/:id/ice
/// With ICE-lite on the server, trickle ICE is a no-op but the endpoint must exist.
pub async fn ice_candidate(
    State(_state): State<Arc<AppState>>,
    Extension(_user): Extension<AuthUser>,
    Path(_session_id): Path<String>,
) -> Response {
    StatusCode::OK.into_response()
}
