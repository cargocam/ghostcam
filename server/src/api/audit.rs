use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use serde::{Deserialize, Serialize};

use super::auth::AuthUser;
use super::state::AppState;

#[derive(Deserialize)]
pub struct AuditQueryParams {
    /// Filter by event type (e.g. "auth_success", "camera_connected")
    #[serde(rename = "type")]
    pub event_type: Option<String>,
    /// Only return entries at or after this RFC3339 timestamp
    pub since: Option<String>,
    /// Only return entries at or before this RFC3339 timestamp
    pub until: Option<String>,
    /// Maximum number of entries to return (default 100, max 1000)
    pub limit: Option<i64>,
    /// Number of entries to skip for pagination
    pub offset: Option<i64>,
}

#[derive(Serialize)]
pub struct AuditEntryResponse {
    pub id: i64,
    pub timestamp: String,
    pub event_type: String,
    pub event_data: serde_json::Value,
    pub hmac: String,
}

#[derive(Serialize)]
pub struct AuditQueryResponse {
    pub entries: Vec<AuditEntryResponse>,
    pub total: i64,
}

/// GET /api/v1/audit
// TODO: Restrict to admin role once a role system exists. Currently all
// authenticated users can access audit logs (single-user system).
pub async fn query(
    State(state): State<Arc<AppState>>,
    Extension(_user): Extension<AuthUser>,
    Query(params): Query<AuditQueryParams>,
) -> Response {
    let limit = params.limit.unwrap_or(100).clamp(1, 1000);
    let offset = params.offset.unwrap_or(0).max(0);

    // Validate timestamps before hitting the database
    if let Some(ref s) = params.since {
        if chrono::DateTime::parse_from_rfc3339(s).is_err() {
            return (
                StatusCode::BAD_REQUEST,
                "invalid 'since' timestamp (expected RFC3339)",
            )
                .into_response();
        }
    }
    if let Some(ref s) = params.until {
        if chrono::DateTime::parse_from_rfc3339(s).is_err() {
            return (
                StatusCode::BAD_REQUEST,
                "invalid 'until' timestamp (expected RFC3339)",
            )
                .into_response();
        }
    }

    let entries = match state
        .db
        .query_audit_log(
            params.event_type.as_deref(),
            params.since.as_deref(),
            params.until.as_deref(),
            limit,
            offset,
        )
        .await
    {
        Ok(records) => records
            .into_iter()
            .map(|r| AuditEntryResponse {
                id: r.id,
                timestamp: r.timestamp,
                event_type: r.event_type,
                event_data: r.event_data,
                hmac: r.hmac,
            })
            .collect(),
        Err(e) => {
            tracing::error!("audit query error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    let total = match state
        .db
        .count_audit_log(
            params.event_type.as_deref(),
            params.since.as_deref(),
            params.until.as_deref(),
        )
        .await
    {
        Ok(t) => t,
        Err(e) => {
            tracing::error!("audit count error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    Json(AuditQueryResponse { entries, total }).into_response()
}
