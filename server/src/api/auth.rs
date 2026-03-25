use std::sync::Arc;

use axum::body::Body;
use axum::extract::State;
use axum::http::{Request, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::types::{SessionId, UserId};
use serde::{Deserialize, Serialize};

use super::state::AppState;
use crate::auth;
use crate::db_trait::NewSession;

/// Authenticated user identity, extracted by middleware.
#[derive(Debug, Clone)]
pub struct AuthUser {
    pub user_id: UserId,
}

/// Auth middleware: checks Bearer token or session cookie.
pub async fn auth_middleware(
    State(state): State<Arc<AppState>>,
    mut request: Request<Body>,
    next: Next,
) -> Response {
    // 1. Check Authorization: Bearer <token>
    if let Some(auth_header) = request.headers().get("authorization") {
        if let Ok(val) = auth_header.to_str() {
            if let Some(token) = val.strip_prefix("Bearer ") {
                let token_hash = auth::hmac_token(token, &state.hmac_secret);
                if let Ok(Some(record)) = state.db.verify_api_token(&token_hash).await {
                    // Check expiry
                    let now = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap()
                        .as_secs();
                    if record.expires_at.is_none()
                        || record.expires_at.unwrap_or(u64::MAX) > now
                    {
                        request
                            .extensions_mut()
                            .insert(AuthUser {
                                user_id: record.user_id,
                            });
                        return next.run(request).await;
                    }
                }
            }
        }
    }

    // 2. Check session cookie
    if let Some(cookie_header) = request.headers().get("cookie") {
        if let Ok(val) = cookie_header.to_str() {
            for cookie in val.split(';') {
                let cookie = cookie.trim();
                if let Some(session_id) = cookie.strip_prefix("ghostcam-session=") {
                    let sid = SessionId(session_id.to_string());
                    if let Ok(Some(session)) = state.db.get_session(&sid).await {
                        let now = std::time::SystemTime::now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .unwrap()
                            .as_secs();
                        if session.expires_at > now {
                            // Extend session TTL
                            let _ = state.db.extend_session(&sid).await;
                            request
                                .extensions_mut()
                                .insert(AuthUser {
                                    user_id: session.user_id,
                                });
                            return next.run(request).await;
                        }
                    }
                }
            }
        }
    }

    StatusCode::UNAUTHORIZED.into_response()
}

#[derive(Deserialize)]
pub struct LoginRequest {
    pub email: String,
    pub password: String,
}

#[derive(Serialize)]
pub struct LoginResponse {
    pub user_id: String,
}

/// POST /api/v1/auth/login
pub async fn login(
    State(state): State<Arc<AppState>>,
    Json(body): Json<LoginRequest>,
) -> Response {
    // Cap password length to prevent Argon2 CPU exhaustion
    if body.password.len() > 128 {
        return StatusCode::UNAUTHORIZED.into_response();
    }

    // Look up user by email
    let user = match state.db.get_user_by_email(&body.email).await {
        Ok(Some(u)) => u,
        Ok(None) => return StatusCode::UNAUTHORIZED.into_response(),
        Err(e) => {
            tracing::error!("failed to look up user: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Check if user is disabled
    if user.disabled_at.is_some() {
        return StatusCode::UNAUTHORIZED.into_response();
    }

    // Verify password
    match state.db.verify_password(&user.user_id, &body.password).await {
        Ok(true) => {}
        _ => return StatusCode::UNAUTHORIZED.into_response(),
    }

    // Create session
    let session_id = auth::generate_session_id();
    let new_session = NewSession {
        session_id: session_id.clone(),
        user_id: user.user_id.clone(),
        user_agent: None,
        ip_address: None,
    };
    if let Err(e) = state.db.create_session(&new_session).await {
        tracing::error!("failed to create session: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    let cookie = format!(
        "ghostcam-session={}; Path=/; HttpOnly; SameSite=Strict; Max-Age={}",
        session_id.0,
        ghostcam::config::SESSION_TTL_DAYS * 86400,
    );

    Response::builder()
        .status(StatusCode::OK)
        .header("set-cookie", cookie)
        .header("content-type", "application/json")
        .body(Body::from(
            serde_json::to_string(&LoginResponse {
                user_id: user.user_id.0,
            })
            .unwrap(),
        ))
        .unwrap()
}

/// POST /api/v1/auth/logout
pub async fn logout(
    State(state): State<Arc<AppState>>,
    Extension(_user): Extension<AuthUser>,
    request: Request<Body>,
) -> Response {
    // Find and delete session from cookie
    if let Some(cookie_header) = request.headers().get("cookie") {
        if let Ok(val) = cookie_header.to_str() {
            for cookie in val.split(';') {
                let cookie = cookie.trim();
                if let Some(session_id) = cookie.strip_prefix("ghostcam-session=") {
                    let sid = SessionId(session_id.to_string());
                    let _ = state.db.delete_session(&sid).await;
                }
            }
        }
    }

    let cookie = "ghostcam-session=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0";
    Response::builder()
        .status(StatusCode::OK)
        .header("set-cookie", cookie)
        .body(Body::empty())
        .unwrap()
}

#[derive(Deserialize)]
pub struct ChangePasswordRequest {
    pub current_password: String,
    pub new_password: String,
}

/// PATCH /api/v1/auth/password
pub async fn change_password(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(body): Json<ChangePasswordRequest>,
) -> Response {
    // Validate new password length
    if body.new_password.len() < 8 || body.new_password.len() > 128 {
        return (StatusCode::BAD_REQUEST, "password must be 8-128 characters").into_response();
    }

    // Cap current password check to prevent Argon2 CPU exhaustion
    if body.current_password.len() > 128 {
        return StatusCode::UNAUTHORIZED.into_response();
    }

    // Verify current password
    match state
        .db
        .verify_password(&user.user_id, &body.current_password)
        .await
    {
        Ok(true) => {}
        Ok(false) => return StatusCode::UNAUTHORIZED.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    // Hash new password
    let new_hash = match auth::hash_password(&body.new_password) {
        Ok(h) => h,
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    };

    // Set new password
    if let Err(e) = state.db.set_password(&user.user_id, &new_hash).await {
        tracing::error!("failed to set password: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    StatusCode::OK.into_response()
}

#[derive(Deserialize)]
pub struct RegisterRequest {
    pub email: String,
    pub password: String,
    pub display_name: Option<String>,
}

#[derive(Serialize)]
pub struct RegisterResponse {
    pub user_id: String,
}

/// POST /api/v1/auth/register
pub async fn register(
    State(state): State<Arc<AppState>>,
    Json(body): Json<RegisterRequest>,
) -> Response {
    // Validate email
    if !body.email.contains('@') || body.email.len() > 254 {
        return (StatusCode::BAD_REQUEST, "invalid email address").into_response();
    }

    // Validate password length (max 128 to prevent Argon2 CPU exhaustion)
    if body.password.len() < 8 || body.password.len() > 128 {
        return (StatusCode::BAD_REQUEST, "password must be 8-128 characters").into_response();
    }

    // Check if email is already taken
    match state.db.get_user_by_email(&body.email).await {
        Ok(Some(_)) => {
            return (StatusCode::CONFLICT, "email already registered").into_response();
        }
        Ok(None) => {}
        Err(e) => {
            tracing::error!("failed to check email: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // Hash password
    let hash = match auth::hash_password(&body.password) {
        Ok(h) => h,
        Err(e) => {
            tracing::error!("failed to hash password: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Create user (handle concurrent duplicate email via DB unique constraint)
    let display_name = body.display_name.as_deref().unwrap_or("User");
    let user_id = match state.db.create_user(&body.email, &hash, display_name).await {
        Ok(id) => id,
        Err(e) => {
            // Check for PostgreSQL unique_violation (SQLSTATE 23505)
            if let Some(db_err) = e.downcast_ref::<sqlx::Error>() {
                if let sqlx::Error::Database(db_err) = db_err {
                    if db_err.code().as_deref() == Some("23505") {
                        return (StatusCode::CONFLICT, "email already registered").into_response();
                    }
                }
            }
            tracing::error!("failed to create user: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Auto-login: create session
    let session_id = auth::generate_session_id();
    let new_session = NewSession {
        session_id: session_id.clone(),
        user_id: user_id.clone(),
        user_agent: None,
        ip_address: None,
    };
    if let Err(e) = state.db.create_session(&new_session).await {
        tracing::error!("failed to create session: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    let cookie = format!(
        "ghostcam-session={}; Path=/; HttpOnly; SameSite=Strict; Max-Age={}",
        session_id.0,
        ghostcam::config::SESSION_TTL_DAYS * 86400,
    );

    Response::builder()
        .status(StatusCode::CREATED)
        .header("set-cookie", cookie)
        .header("content-type", "application/json")
        .body(Body::from(
            serde_json::to_string(&RegisterResponse {
                user_id: user_id.0,
            })
            .unwrap(),
        ))
        .unwrap()
}
