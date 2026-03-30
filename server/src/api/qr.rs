use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use serde::Deserialize;

use super::auth::AuthUser;
use super::state::AppState;

/// Default claim token TTL: 24 hours.
const DEFAULT_TTL_HOURS: u64 = 24;
/// Maximum allowed TTL: 7 days.
const MAX_TTL_HOURS: u64 = 7 * 24;

#[derive(Deserialize)]
pub struct QrRequest {
    /// WiFi SSID to embed in QR (optional).
    pub wifi_ssid: Option<String>,
    /// WiFi password to embed in QR (optional).
    pub wifi_password: Option<String>,
    /// Token time-to-live in hours (default 24, max 168).
    pub ttl_hours: Option<u64>,
}

/// POST /api/v1/cameras/enroll/qr
///
/// Generates a stateless claim JWT with the user's ID embedded as `sub`,
/// and returns a QR code SVG containing the token, server address, and
/// optional WiFi credentials.
///
/// The token is multi-use: any number of unclaimed cameras can scan it.
/// No database write is needed.
pub async fn enrollment_qr(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    body: Option<Json<QrRequest>>,
) -> Response {
    let body = body.map(|Json(b)| b);

    let ttl_hours = body
        .as_ref()
        .and_then(|b| b.ttl_hours)
        .unwrap_or(DEFAULT_TTL_HOURS)
        .clamp(1, MAX_TTL_HOURS);
    let ttl_secs = ttl_hours * 3600;

    // Generate stateless claim JWT with sub = user_id
    let claims = crate::pki::enrollment::EnrollmentClaims::new_claim(
        &state.enrollment_addr,
        &user.user_id.0,
        ttl_secs,
    );

    // Sign the JWT
    let jwt = match state.ca.sign_enrollment_jwt(&claims) {
        Ok(t) => t,
        Err(e) => {
            tracing::error!("failed to sign enrollment JWT: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Build QR payload
    let mut payload = serde_json::Map::new();

    // Add WiFi credentials if provided
    if let Some(ref b) = body {
        if let Some(ref ssid) = b.wifi_ssid {
            if !ssid.is_empty() {
                payload.insert("w".into(), serde_json::Value::String(ssid.clone()));
                if let Some(ref psk) = b.wifi_password {
                    payload.insert("p".into(), serde_json::Value::String(psk.clone()));
                }
            }
        }
    }

    // Server address (includes port for non-standard deployments)
    payload.insert(
        "s".into(),
        serde_json::Value::String(state.enrollment_addr.clone()),
    );

    // Claim token
    payload.insert("t".into(), serde_json::Value::String(jwt));

    let payload_str = serde_json::Value::Object(payload).to_string();

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
            device_id: format!("qr-claim-{}", &claims.jti[..8]),
            owner_id: user.user_id.0.clone(),
        });

    (StatusCode::OK, [("content-type", "image/svg+xml")], svg).into_response()
}
