use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

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
/// Generates a one-time provision token, stores its HMAC hash in the database,
/// and returns a QR code SVG containing the raw token, server URL, and optional
/// WiFi credentials.
pub async fn enrollment_qr(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    axum::extract::Host(host): axum::extract::Host,
    body: Option<Json<QrRequest>>,
) -> Response {
    let body = body.map(|Json(b)| b);

    let ttl_hours = body
        .as_ref()
        .and_then(|b| b.ttl_hours)
        .unwrap_or(DEFAULT_TTL_HOURS)
        .clamp(1, MAX_TTL_HOURS);
    let ttl_secs = ttl_hours * 3600;

    // Generate a one-time provision token
    let raw_token = crate::auth::generate_random_password();
    let token_hash = crate::auth::hmac_token(&raw_token, &state.hmac_secret);

    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let expires_at = now + ttl_secs;

    if let Err(e) = state
        .db
        .create_provision_token(&token_hash, &user.user_id, expires_at)
        .await
    {
        tracing::error!("failed to create provision token: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

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

    // Server URL derived from the request Host header
    let server_url = format!("https://{host}");
    payload.insert("s".into(), serde_json::Value::String(server_url));

    // Provision token
    payload.insert("t".into(), serde_json::Value::String(raw_token));

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
            device_id: format!("qr-provision-{}", &token_hash[..8]),
            owner_id: user.user_id.0.clone(),
        });

    (StatusCode::OK, [("content-type", "image/svg+xml")], svg).into_response()
}
