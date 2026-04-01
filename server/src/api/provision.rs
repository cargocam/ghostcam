use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use ghostcam::api_types::{ProvisionRequest, ProvisionResponse};
use ghostcam::types::DeviceId;

use super::state::AppState;
use crate::auth;

/// POST /api/v1/cameras/provision
///
/// Public endpoint (rate-limited). Camera sends a one-time provisioning token
/// (from QR code) plus its device serial. Server validates, creates a camera
/// record, generates an API key, and returns it.
pub async fn provision(
    State(state): State<Arc<AppState>>,
    Json(body): Json<ProvisionRequest>,
) -> Response {
    if body.token.is_empty() || body.device_serial.is_empty() {
        return (StatusCode::BAD_REQUEST, "token and device_serial required").into_response();
    }

    // Hash the token to look it up
    let token_hash = auth::hmac_token(&body.token, &state.hmac_secret);

    // Create the camera device_id
    let device_id = DeviceId(uuid::Uuid::new_v4().to_string());

    // Claim the provision token (atomic: checks not expired, not already claimed)
    let user_id = match state.db.claim_provision_token(&token_hash, &device_id).await {
        Ok(Some(uid)) => uid,
        Ok(None) => {
            return (StatusCode::UNAUTHORIZED, "invalid or expired provisioning token")
                .into_response();
        }
        Err(e) => {
            tracing::error!("provision token claim failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Check camera limit (billing enforcement)
    match crate::billing::enforcement::check_camera_limit(
        state.db.as_ref(),
        &user_id,
        &state.tiers,
        state.stripe.is_some(),
    )
    .await
    {
        Ok(Ok(())) => {}
        Ok(Err(_e)) => {
            return (
                StatusCode::PAYMENT_REQUIRED,
                Json(serde_json::json!({"error": "camera_limit_reached"})),
            )
                .into_response();
        }
        Err(e) => {
            tracing::error!("billing check failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // Create camera record
    if let Err(e) = state
        .db
        .create_provisioned_camera(&device_id, &user_id, &body.device_serial)
        .await
    {
        // Check for duplicate device_serial (unique constraint violation)
        if let Some(sqlx::Error::Database(db_err)) = e.downcast_ref::<sqlx::Error>() {
            if db_err.code().as_deref() == Some("23505") {
                return (StatusCode::CONFLICT, "device already provisioned").into_response();
            }
        }
        tracing::error!("failed to create camera: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    // Generate and store API key
    let api_key = auth::generate_random_password();
    let api_key_hash = auth::hmac_token(&api_key, &state.hmac_secret);

    if let Err(e) = state
        .db
        .create_camera_api_key(&device_id, &api_key_hash)
        .await
    {
        tracing::error!("failed to create camera API key: {e}");
        let _ = state.db.delete_camera(&device_id).await;
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    state.audit.log(crate::audit::AuditEvent::CameraProvisioned {
        device_id: device_id.0.clone(),
        user_id: user_id.0.clone(),
        device_serial: body.device_serial.clone(),
    });

    Json(ProvisionResponse {
        api_key,
        device_id: device_id.0,
    })
    .into_response()
}
