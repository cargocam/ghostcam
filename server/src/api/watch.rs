use std::net::IpAddr;
use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::types::DeviceId;
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use super::auth::AuthUser;
use super::state::AppState;
use crate::egress::handle::EgressHandle;

#[derive(Deserialize)]
pub struct WatchRequest {
    pub sdp_offer: String,
    pub device_id: String,
}

#[derive(Serialize)]
pub struct WatchResponse {
    pub session_id: String,
    pub sdp_answer: String,
}

/// POST /api/v1/watch
pub async fn create_session(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    headers: HeaderMap,
    Json(body): Json<WatchRequest>,
) -> Response {
    let device_id = DeviceId(body.device_id);

    // Verify camera belongs to user
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        Ok(Some(_)) | Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    }

    // Enforce session limits
    if !state.sessions.can_create_for_user(&user.user_id).await {
        return (StatusCode::TOO_MANY_REQUESTS, "too many sessions for user").into_response();
    }
    if !state.sessions.can_create_for_device(&device_id).await {
        return (StatusCode::TOO_MANY_REQUESTS, "too many viewers for camera").into_response();
    }

    // Look up slot
    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => {
            return (StatusCode::CONFLICT, "camera is offline").into_response();
        }
    };

    // Determine the ICE candidate IP.
    let ice_ip = resolve_ice_ip(state.public_ip_override, &headers);
    tracing::info!(ip = %ice_ip, "ICE candidate IP for session");

    // Create egress handle
    let session_id = uuid::Uuid::new_v4().to_string();
    let cancel = CancellationToken::new();

    let (egress, sdp_answer) = match EgressHandle::create(
        session_id.clone(),
        &slot,
        &body.sdp_offer,
        state.webrtc_socket.clone(),
        ice_ip,
        cancel.clone(),
    )
    .await
    {
        Ok(r) => r,
        Err(e) => {
            tracing::error!("failed to create egress handle: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Spawn the egress event loop
    let handle = tokio::spawn(async move {
        if let Err(e) = egress.run().await {
            tracing::warn!("egress session ended with error: {e}");
        }
    });

    // Register session
    state
        .sessions
        .register(
            session_id.clone(),
            device_id,
            user.user_id,
            cancel,
            handle,
        )
        .await;

    Json(WatchResponse {
        session_id,
        sdp_answer,
    })
    .into_response()
}

/// DELETE /api/v1/session/:id
pub async fn teardown_session(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(session_id): Path<String>,
) -> Response {
    // Verify ownership
    match state.sessions.get_user_id(&session_id).await {
        Some(uid) if uid == user.user_id => {}
        Some(_) => return StatusCode::FORBIDDEN.into_response(),
        None => return StatusCode::NOT_FOUND.into_response(),
    }

    state.sessions.teardown(&session_id).await;
    StatusCode::OK.into_response()
}

/// POST /api/v1/session/:id/ice
/// With ICE-lite on the server, trickle ICE is a no-op but the endpoint must exist.
pub async fn ice_candidate(
    State(_state): State<Arc<AppState>>,
    Extension(_user): Extension<AuthUser>,
    Path(_session_id): Path<String>,
) -> Response {
    StatusCode::OK.into_response()
}

/// Determine the IP address to advertise in ICE candidates.
///
/// Priority:
/// 1. Explicit override (`GHOSTCAM_PUBLIC_IP`) — always wins.
/// 2. HTTP `Host` header hostname — the browser reached us at this address,
///    so UDP port-mapped on the same host will be reachable too.
/// 3. `127.0.0.1` fallback.
fn resolve_ice_ip(public_ip_override: Option<IpAddr>, headers: &HeaderMap) -> IpAddr {
    // 1. Explicit override
    if let Some(ip) = public_ip_override {
        return ip;
    }

    // 2. Derive from Host header
    if let Some(host_val) = headers.get("host").and_then(|v| v.to_str().ok()) {
        // Strip port: "localhost:5173" → "localhost", "[::1]:3000" → "::1"
        let hostname = if host_val.starts_with('[') {
            // IPv6 literal: [::1]:port
            host_val
                .find(']')
                .map(|i| &host_val[1..i])
                .unwrap_or(host_val)
        } else {
            host_val.split(':').next().unwrap_or(host_val)
        };

        // Try parsing as IP literal first
        if let Ok(ip) = hostname.parse::<IpAddr>() {
            return ip;
        }

        // Resolve hostname → IP
        use std::net::ToSocketAddrs;
        if let Ok(mut addrs) = (hostname, 0u16).to_socket_addrs() {
            if let Some(addr) = addrs.find(|a| a.ip().is_ipv4()) {
                return addr.ip();
            }
        }
    }

    // 3. Fallback
    IpAddr::V4(std::net::Ipv4Addr::LOCALHOST)
}
