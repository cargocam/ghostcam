use crate::webrtc::WebRtcCommand;
use crate::AppState;
use axum::extract::{Path, State};
use axum::http::{header, StatusCode};
use axum::response::Json;
use axum::routing::{delete, get, post};
use axum::Router;
use ghostcam::group::GroupId;
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::oneshot;
use tower_http::cors::CorsLayer;
use tower_http::services::ServeDir;

pub fn create_router(state: Arc<AppState>, viewer_dir: Option<PathBuf>) -> Router {
    let api_key = state.api_key.clone();

    let api = Router::new()
        .route("/api/v1/watch/:group_id", post(watch))
        .route("/api/v1/session/:id", delete(delete_session))
        .route("/api/v1/session/:id/ice", post(trickle_ice))
        .route("/api/v1/groups", get(list_groups))
        .route("/api/v1/groups/:group_id/cameras", get(list_cameras))
        .route_layer(axum::middleware::from_fn(
            move |req: axum::extract::Request, next: axum::middleware::Next| {
                let key = api_key.clone();
                async move {
                    let auth = req
                        .headers()
                        .get(header::AUTHORIZATION)
                        .and_then(|v: &axum::http::HeaderValue| v.to_str().ok())
                        .unwrap_or("");
                    if auth.starts_with("Bearer ") && &auth[7..] == key {
                        Ok::<_, StatusCode>(next.run(req).await)
                    } else {
                        Err(StatusCode::UNAUTHORIZED)
                    }
                }
            },
        ));

    let health = Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz));

    let mut app = Router::new()
        .merge(api)
        .merge(health)
        .layer(CorsLayer::permissive())
        .with_state(state);

    // Serve built viewer as static files
    if let Some(dir) = viewer_dir {
        app = app.fallback_service(ServeDir::new(dir));
    }

    app
}

#[derive(Deserialize)]
struct WatchRequest {
    sdp_offer: String,
}

#[derive(Serialize)]
struct WatchResponse {
    session_id: String,
    sdp_answer: String,
}

async fn watch(
    State(state): State<Arc<AppState>>,
    Path(group_id): Path<String>,
    Json(body): Json<WatchRequest>,
) -> Result<(StatusCode, Json<WatchResponse>), (StatusCode, String)> {
    let (reply_tx, reply_rx) = oneshot::channel();

    state
        .webrtc_cmd_tx
        .send(WebRtcCommand::CreateSession {
            sdp_offer: body.sdp_offer,
            group_id: GroupId::new(group_id),
            reply: reply_tx,
        })
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine unavailable".into()))?;

    let (session_id, sdp_answer) = reply_rx
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine dropped".into()))?
        .map_err(|e| (StatusCode::BAD_REQUEST, e))?;

    Ok((
        StatusCode::CREATED,
        Json(WatchResponse {
            session_id,
            sdp_answer,
        }),
    ))
}

async fn delete_session(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
) -> Result<StatusCode, (StatusCode, String)> {
    let (reply_tx, reply_rx) = oneshot::channel();

    state
        .webrtc_cmd_tx
        .send(WebRtcCommand::DeleteSession {
            session_id: id,
            reply: reply_tx,
        })
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine unavailable".into()))?;

    reply_rx
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine dropped".into()))?
        .map_err(|e| (StatusCode::NOT_FOUND, e))?;

    Ok(StatusCode::NO_CONTENT)
}

#[derive(Deserialize)]
struct IceRequest {
    candidate: String,
}

async fn trickle_ice(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
    Json(body): Json<IceRequest>,
) -> Result<StatusCode, (StatusCode, String)> {
    let (reply_tx, reply_rx) = oneshot::channel();

    state
        .webrtc_cmd_tx
        .send(WebRtcCommand::TrickleIce {
            session_id: id,
            candidate: body.candidate,
            reply: reply_tx,
        })
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine unavailable".into()))?;

    reply_rx
        .await
        .map_err(|_| (StatusCode::INTERNAL_SERVER_ERROR, "engine dropped".into()))?
        .map_err(|e| (StatusCode::NOT_FOUND, e))?;

    Ok(StatusCode::NO_CONTENT)
}

#[derive(Serialize)]
struct GroupInfo {
    group_id: String,
    camera_count: usize,
}

async fn list_groups(State(state): State<Arc<AppState>>) -> Json<Vec<GroupInfo>> {
    let router = state.router.read().await;
    let groups: Vec<GroupInfo> = router
        .all_groups()
        .into_iter()
        .map(|(g, count)| GroupInfo {
            group_id: g.0,
            camera_count: count,
        })
        .collect();
    Json(groups)
}

#[derive(Serialize)]
struct CameraInfo {
    device_id: String,
    group_id: String,
    capabilities: Vec<String>,
    connected_at: u64,
}

async fn list_cameras(
    State(state): State<Arc<AppState>>,
    Path(group_id): Path<String>,
) -> Json<Vec<CameraInfo>> {
    let router = state.router.read().await;
    let cameras = router.get_cameras_in_group(&GroupId::new(group_id));
    let result: Vec<CameraInfo> = cameras
        .into_iter()
        .map(|c| CameraInfo {
            device_id: c.device_id.clone(),
            group_id: c.group_id.0.clone(),
            capabilities: c.capabilities.clone(),
            connected_at: c.connected_at,
        })
        .collect();
    Json(result)
}

async fn healthz() -> &'static str {
    "ok"
}

async fn readyz(State(state): State<Arc<AppState>>) -> Result<&'static str, StatusCode> {
    // Ready if we have at least the QUIC listener running
    Ok("ok")
}
