use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

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
    pub notes: Option<String>,
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

    let responses: Vec<CameraResponse> = cameras
        .into_iter()
        .map(|cam| CameraResponse {
            device_id: cam.device_id.0,
            display_name: cam.display_name,
            enrolled_at: cam.enrolled_at,
            notes: cam.notes,
        })
        .collect();

    Json(responses).into_response()
}

/// POST /api/v1/cameras
pub async fn enroll(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(_body): Json<EnrollRequest>,
) -> Response {
    // Check camera limit (billing enforcement)
    match crate::billing::enforcement::check_camera_limit(
        state.db.as_ref(),
        &user.user_id,
        &state.tiers,
        state.stripe.is_some(),
    )
    .await
    {
        Ok(Ok(())) => {} // allowed
        Ok(Err(e)) => {
            state
                .audit
                .log(crate::audit::AuditEvent::CameraLimitBlocked {
                    user_id: user.user_id.0.clone(),
                    current: match &e {
                        crate::billing::enforcement::EnforcementError::CameraLimitReached {
                            current,
                            ..
                        } => *current,
                        _ => 0,
                    },
                    limit: match &e {
                        crate::billing::enforcement::EnforcementError::CameraLimitReached {
                            limit,
                            ..
                        } => *limit,
                        _ => 0,
                    },
                });
            let error_key = match &e {
                crate::billing::enforcement::EnforcementError::SubscriptionSuspended => {
                    "subscription_suspended"
                }
                _ => "camera_limit_reached",
            };
            return (
                StatusCode::PAYMENT_REQUIRED,
                Json(serde_json::json!({
                    "error": error_key,
                    "message": e.to_string(),
                })),
            )
                .into_response();
        }
        Err(e) => {
            tracing::error!("camera limit check failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // Generate a one-time provision token
    let raw_token = crate::auth::generate_random_password();
    let token_hash = crate::auth::hmac_token(&raw_token, &state.hmac_secret);

    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let expires_at = now + ghostcam::config::PROVISION_TOKEN_TTL_SECS;

    if let Err(e) = state
        .db
        .create_provision_token(&token_hash, &user.user_id, expires_at)
        .await
    {
        tracing::error!("failed to create provision token: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    state
        .audit
        .log(crate::audit::AuditEvent::EnrollmentStarted {
            device_id: format!("provision-{}", &token_hash[..8]),
            owner_id: user.user_id.0.clone(),
        });

    Json(EnrollResponse {
        token: raw_token,
        expires_at,
    })
    .into_response()
}

/// Legacy request body for POST /api/v1/cameras.
/// Fields are accepted for backward compat (Docker entrypoint) but no longer
/// embedded in the JWT -- claim tokens are now stateless with just `sub`.
#[derive(Deserialize)]
#[allow(dead_code)]
pub struct EnrollRequest {
    pub display_name: Option<String>,
    pub wifi: Option<Vec<WifiCredential>>,
}

#[derive(Deserialize, Serialize)]
#[allow(dead_code)]
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
    if camera.user_id.as_ref() != Some(&user.user_id) {
        return StatusCode::NOT_FOUND.into_response();
    }

    Json(CameraResponse {
        device_id: camera.device_id.0,
        display_name: camera.display_name,
        enrolled_at: camera.enrolled_at,
        notes: camera.notes,
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        Ok(Some(_)) | Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    if let Err(e) = state.db.delete_camera(&device_id).await {
        tracing::error!("delete camera error: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    state
        .audit
        .log(crate::audit::AuditEvent::CameraUnregistered {
            device_id: device_id.0,
            initiated_by: user.user_id.0,
        });

    StatusCode::OK.into_response()
}
