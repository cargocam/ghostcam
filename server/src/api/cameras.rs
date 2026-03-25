use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::types::DeviceId;
use serde::{Deserialize, Serialize};

use super::auth::AuthUser;
use super::state::AppState;
use crate::db_trait::CameraUpdate;

#[derive(Serialize)]
pub struct CameraResponse {
    pub device_id: String,
    pub display_name: String,
    pub enrolled_at: u64,
    pub last_seen_at: Option<u64>,
    pub notes: Option<String>,
    pub online: bool,
}

#[derive(Serialize)]
pub struct EnrollResponse {
    pub token: String,
    pub expires_at: u64,
}

/// GET /api/v1/cameras
pub async fn list(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Response {
    let cameras = match state.db.list_cameras(&user.user_id).await {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("list cameras error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    let mut responses = Vec::new();
    for cam in cameras {
        let online = state.registry.is_connected(&cam.device_id).await;
        responses.push(CameraResponse {
            device_id: cam.device_id.0,
            display_name: cam.display_name,
            enrolled_at: cam.enrolled_at,
            last_seen_at: cam.last_seen_at,
            notes: cam.notes,
            online,
        });
    }

    Json(responses).into_response()
}

/// POST /api/v1/cameras
pub async fn enroll(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(body): Json<EnrollRequest>,
) -> Response {
    let claims = crate::pki::enrollment::EnrollmentClaims::new(
        &state.enrollment_addr,
        body.display_name,
        body.wifi.map(|w| {
            w.into_iter()
                .map(|c| crate::pki::enrollment::WifiCredential {
                    ssid: c.ssid,
                    psk: c.psk,
                })
                .collect()
        }),
    );
    let expires_at = claims.exp;
    let jti = claims.jti.clone();

    // Store enrollment token in DB
    let token_record = crate::db_trait::NewEnrollmentToken {
        jti: jti.clone(),
        user_id: user.user_id.clone(),
        expires_at,
    };
    if let Err(e) = state.db.create_enrollment_token(&token_record).await {
        tracing::error!("failed to store enrollment token: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    // Sign JWT
    let token = match state.ca.sign_enrollment_jwt(&claims) {
        Ok(t) => t,
        Err(e) => {
            tracing::error!("failed to sign enrollment JWT: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    Json(EnrollResponse { token, expires_at }).into_response()
}

#[derive(Deserialize)]
pub struct EnrollRequest {
    pub display_name: Option<String>,
    pub wifi: Option<Vec<WifiCredential>>,
}

#[derive(Deserialize, Serialize)]
pub struct WifiCredential {
    pub ssid: String,
    pub psk: String,
}

/// GET /api/v1/cameras/:device_id
pub async fn get(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);
    let camera = match state.db.get_camera(&device_id).await {
        Ok(Some(c)) => c,
        Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(e) => {
            tracing::error!("get camera error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Verify ownership
    if camera.user_id != user.user_id {
        return StatusCode::NOT_FOUND.into_response();
    }

    let online = state.registry.is_connected(&device_id).await;
    Json(CameraResponse {
        device_id: camera.device_id.0,
        display_name: camera.display_name,
        enrolled_at: camera.enrolled_at,
        last_seen_at: camera.last_seen_at,
        notes: camera.notes,
        online,
    })
    .into_response()
}

#[derive(Deserialize)]
pub struct UpdateCameraRequest {
    pub display_name: Option<String>,
    pub notes: Option<String>,
}

/// PATCH /api/v1/cameras/:device_id
pub async fn update(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
    Json(body): Json<UpdateCameraRequest>,
) -> Response {
    let device_id = DeviceId(device_id);

    // Verify ownership
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        Ok(Some(_)) | Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    let update = CameraUpdate {
        display_name: body.display_name,
        notes: body.notes,
    };

    if let Err(e) = state.db.update_camera(&device_id, &update).await {
        tracing::error!("update camera error: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    StatusCode::OK.into_response()
}

/// DELETE /api/v1/cameras/:device_id
pub async fn delete(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    // Verify ownership
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        Ok(Some(_)) | Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    match crate::pki::unregister::unregister_camera(
        &device_id,
        &state.registry,
        state.db.as_ref(),
        &state.revocation_cache,
        state.redis.as_deref(),
    )
    .await
    {
        Ok(_) => {
            // Teardown any WebRTC sessions
            state.sessions.teardown_by_device(&device_id).await;
            StatusCode::OK.into_response()
        }
        Err(e) => {
            tracing::error!("delete camera error: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}
