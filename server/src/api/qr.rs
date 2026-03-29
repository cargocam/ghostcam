use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
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
    headers: HeaderMap,
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

    // Derive server base URL from the request's Host header
    let server_url = derive_server_url(&headers, state.public_ip_override);

    // Build QR payload
    let payload = serde_json::json!({
        "s": server_url,
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

/// Derive the server's HTTP base URL from the request headers.
///
/// Uses the Host header to construct a URL the camera can reach. If a public IP
/// override is set, uses that instead. Respects `X-Forwarded-Proto` to use HTTPS
/// when behind a reverse proxy.
fn derive_server_url(headers: &HeaderMap, public_ip_override: Option<std::net::IpAddr>) -> String {
    let scheme = headers
        .get("x-forwarded-proto")
        .and_then(|v| v.to_str().ok())
        .filter(|s| *s == "https")
        .unwrap_or("http");

    if let Some(ip) = public_ip_override {
        // If there's an explicit public IP, use it with the port from Host header
        let port = headers
            .get("host")
            .and_then(|v| v.to_str().ok())
            .and_then(|h| h.rsplit(':').next())
            .and_then(|p| p.parse::<u16>().ok());

        return match port {
            Some(p) => format!("{scheme}://{ip}:{p}"),
            None => format!("{scheme}://{ip}:3000"),
        };
    }

    // Fall back to Host header
    if let Some(host) = headers.get("host").and_then(|v| v.to_str().ok()) {
        return format!("{scheme}://{host}");
    }

    // Last resort
    "http://127.0.0.1:3000".to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derive_url_from_host_header() {
        let mut headers = HeaderMap::new();
        headers.insert("host", "10.0.0.5:3000".parse().unwrap());
        let url = derive_server_url(&headers, None);
        assert_eq!(url, "http://10.0.0.5:3000");
    }

    #[test]
    fn derive_url_with_public_ip_override() {
        let mut headers = HeaderMap::new();
        headers.insert("host", "localhost:5173".parse().unwrap());
        let ip: std::net::IpAddr = "192.168.1.50".parse().unwrap();
        let url = derive_server_url(&headers, Some(ip));
        // Should use the override IP with port from Host
        assert_eq!(url, "http://192.168.1.50:5173");
    }

    #[test]
    fn derive_url_public_ip_no_port_in_host() {
        let mut headers = HeaderMap::new();
        headers.insert("host", "example.com".parse().unwrap());
        let ip: std::net::IpAddr = "10.0.0.1".parse().unwrap();
        let url = derive_server_url(&headers, Some(ip));
        // "example.com" doesn't parse as u16, so falls back to 3000
        assert_eq!(url, "http://10.0.0.1:3000");
    }

    #[test]
    fn derive_url_no_host_no_override() {
        let headers = HeaderMap::new();
        let url = derive_server_url(&headers, None);
        assert_eq!(url, "http://127.0.0.1:3000");
    }
}
