use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;

use super::auth::AuthUser;
use super::state::AppState;

/// GET /api/v1/cameras/enroll/qr
///
/// Generates an enrollment JWT, encodes it with the server URL into a QR code,
/// and returns the QR code as an SVG image.
pub async fn enrollment_qr(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Response {
    // Check camera limit before generating a token
    match crate::billing::enforcement::check_camera_limit(
        state.db.as_ref(),
        &user.user_id,
        &state.tiers,
        state.stripe.is_some(),
    )
    .await
    {
        Ok(Ok(())) => {}
        Ok(Err(e)) => {
            return (
                StatusCode::PAYMENT_REQUIRED,
                serde_json::json!({ "error": e.to_string() }).to_string(),
            )
                .into_response();
        }
        Err(e) => {
            tracing::error!("camera limit check failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // Generate enrollment claims (same as POST /api/v1/cameras)
    let claims = crate::pki::enrollment::EnrollmentClaims::new(
        &state.enrollment_addr,
        None, // QR enrollment doesn't carry display_name
        None, // or wifi credentials
    );

    let jti = claims.jti.clone();
    let expires_at = claims.exp;

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

    // Sign the JWT
    let jwt = match state.ca.sign_enrollment_jwt(&claims) {
        Ok(t) => t,
        Err(e) => {
            tracing::error!("failed to sign enrollment JWT: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Build QR payload — just the claim token (camera is already connected to server)
    let payload = serde_json::json!({
        "t": jwt,
    });
    let payload_str = payload.to_string();

    // Generate QR code SVG
    let code = match qrcode::QrCode::new(payload_str.as_bytes()) {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("failed to generate QR code: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    let svg = code
        .render::<qrcode::render::svg::Color>()
        .min_dimensions(200, 200)
        .build();

    state
        .audit
        .log(crate::audit::AuditEvent::EnrollmentStarted {
            device_id: jti,
            owner_id: user.user_id.0.clone(),
        });

    (StatusCode::OK, [("content-type", "image/svg+xml")], svg).into_response()
}
