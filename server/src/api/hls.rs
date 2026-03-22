use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use bytes::Bytes;
use ghostcam::types::DeviceId;
use ghostcam::wire::command::Command;
use tokio::sync::oneshot;
use tokio::time::{timeout, Duration};

use super::auth::AuthUser;
use super::state::AppState;
use crate::ingest::slot::SegmentState;
fn normalize_manifest_for_browser(manifest: &str) -> String {
    manifest
        .lines()
        .map(|line| {
            if line.is_empty() || line.starts_with('#') {
                return line.to_string();
            }
            // Legacy manifests may emit segment IDs like "<device>:<epoch>.m4s".
            // Browsers parse those as URL schemes unless explicitly relative.
            if line.ends_with(".m4s")
                && !line.starts_with("./")
                && !line.starts_with("../")
                && !line.starts_with('/')
                && !line.contains("://")
            {
                format!("./{line}")
            } else {
                line.to_string()
            }
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// GET /hls/:device_id/playlist.m3u8
pub async fn get_manifest(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    // Verify ownership
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    let manifest = slot.manifest.read().await;
    match manifest.as_ref() {
        Some(m) => Response::builder()
            .status(StatusCode::OK)
            .header("content-type", "application/vnd.apple.mpegurl")
            .header("cache-control", "no-cache")
            .body(axum::body::Body::from(normalize_manifest_for_browser(m)))
            .unwrap(),
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

/// GET /hls/:device_id/init.mp4
pub async fn get_init(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    // Check if init segment is already cached
    {
        let init = slot.init_segment.read().await;
        if let Some(data) = init.as_ref() {
            return Response::builder()
                .status(StatusCode::OK)
                .header("content-type", "video/mp4")
                .header("cache-control", "no-store, no-cache, must-revalidate")
                .body(axum::body::Body::from(data.clone()))
                .unwrap();
        }
    }

    // Request upload from camera
    let _ = slot
        .send_command(Command::UploadInit {
            seq: slot.next_seq(),
        })
        .await;

    // Wait for init segment to arrive (with timeout)
    let deadline = Duration::from_secs(10);
    let start = tokio::time::Instant::now();
    loop {
        tokio::time::sleep(Duration::from_millis(100)).await;
        let init = slot.init_segment.read().await;
        if let Some(data) = init.as_ref() {
            return Response::builder()
                .status(StatusCode::OK)
                .header("content-type", "video/mp4")
                .header("cache-control", "no-store, no-cache, must-revalidate")
                .body(axum::body::Body::from(data.clone()))
                .unwrap();
        }
        if start.elapsed() > deadline {
            return StatusCode::GATEWAY_TIMEOUT.into_response();
        }
    }
}

/// GET /hls/:device_id/:segment_id
pub async fn get_segment(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path((device_id, segment_id)): Path<(String, String)>,
) -> Response {
    let device_id = DeviceId(device_id);

    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    // Check if segment is already buffered
    {
        let segments = slot.segments.read().await;
        if let Some(SegmentState::Buffered { data, .. }) = segments.get(&segment_id) {
            return segment_response(data.clone());
        }
    }

    // Set up waiter or join existing upload
    let rx = {
        let mut segments = slot.segments.write().await;
        match segments.get_mut(&segment_id) {
            Some(SegmentState::Uploading { waiters }) => {
                // Another request already triggered the upload — just wait
                let (tx, rx) = oneshot::channel();
                waiters.push(tx);
                rx
            }
            Some(SegmentState::Buffered { data, .. }) => {
                return segment_response(data.clone());
            }
            None => {
                // First request — trigger upload
                let (tx, rx) = oneshot::channel();
                segments.insert(
                    segment_id.clone(),
                    SegmentState::Uploading { waiters: vec![tx] },
                );

                // Send upload command to camera
                let _ = slot
                    .send_command(Command::UploadSegment {
                        seq: slot.next_seq(),
                        segment_id: segment_id.clone(),
                    })
                    .await;

                rx
            }
        }
    };

    // Wait for the upload to complete
    match timeout(Duration::from_secs(30), rx).await {
        Ok(Ok(Ok(data))) => segment_response(data),
        Ok(Ok(Err(e))) => {
            tracing::warn!(device_id = %device_id, segment_id, "segment upload failed: {e}");
            StatusCode::NOT_FOUND.into_response()
        }
        Ok(Err(_)) => StatusCode::INTERNAL_SERVER_ERROR.into_response(),
        Err(_) => StatusCode::GATEWAY_TIMEOUT.into_response(),
    }
}

fn segment_response(data: Bytes) -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "video/mp4")
        .header("cache-control", "private, max-age=3600")
        .body(axum::body::Body::from(data))
        .unwrap()
}
