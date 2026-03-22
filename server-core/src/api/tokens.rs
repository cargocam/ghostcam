use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use serde::{Deserialize, Serialize};

use super::auth::AuthUser;
use super::state::AppState;
use crate::auth;
use crate::db::NewApiToken;

#[derive(Serialize)]
pub struct TokenResponse {
    pub token_id: String,
    pub label: String,
    pub created_at: u64,
    pub expires_at: Option<u64>,
    pub last_used_at: Option<u64>,
}

#[derive(Serialize)]
pub struct CreateTokenResponse {
    pub token_id: String,
    pub raw_token: String,
}

#[derive(Deserialize)]
pub struct CreateTokenRequest {
    pub label: String,
    pub expires_at: Option<u64>,
}

/// GET /api/v1/tokens
pub async fn list(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Response {
    match state.db.list_api_tokens(&user.user_id).await {
        Ok(tokens) => {
            let responses: Vec<TokenResponse> = tokens
                .into_iter()
                .map(|t| TokenResponse {
                    token_id: t.token_id.0,
                    label: t.label,
                    created_at: t.created_at,
                    expires_at: t.expires_at,
                    last_used_at: t.last_used_at,
                })
                .collect();
            Json(responses).into_response()
        }
        Err(e) => {
            tracing::error!("list tokens error: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

/// POST /api/v1/tokens
pub async fn create(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(body): Json<CreateTokenRequest>,
) -> Response {
    let (token_id, raw_token) = auth::generate_api_token();
    let token_hash = auth::hmac_token(&raw_token, &state.hmac_secret);

    let new_token = NewApiToken {
        token_id: token_id.clone(),
        user_id: user.user_id,
        token_hash,
        label: body.label,
        expires_at: body.expires_at,
    };

    if let Err(e) = state.db.create_api_token(&new_token).await {
        tracing::error!("create token error: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    Json(CreateTokenResponse {
        token_id: token_id.0,
        raw_token,
    })
    .into_response()
}

/// DELETE /api/v1/tokens/:token_id
pub async fn revoke(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(token_id): Path<String>,
) -> Response {
    let tid = ghostcam::types::TokenId(token_id);

    // Verify ownership by checking the token belongs to the user
    match state.db.list_api_tokens(&user.user_id).await {
        Ok(tokens) => {
            if !tokens.iter().any(|t| t.token_id == tid) {
                return StatusCode::NOT_FOUND.into_response();
            }
        }
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    if let Err(e) = state.db.delete_api_token(&tid).await {
        tracing::error!("revoke token error: {e}");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    StatusCode::OK.into_response()
}
