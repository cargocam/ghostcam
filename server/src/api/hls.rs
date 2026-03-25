use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use bytes::Bytes;
use ghostcam::types::DeviceId;
use ghostcam::wire::command::Command;
use serde::Serialize;
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

/// Parse segment time windows from an HLS manifest.
/// Returns `(segment_id, start_ms, end_ms)` for each media segment.
fn parse_manifest_segments(manifest: &str) -> Vec<CoverageSegment> {
    let mut segments = Vec::new();
    let mut pending_duration_ms: Option<u64> = None;

    for line in manifest.lines() {
        let line = line.trim();
        if let Some(dur_str) = line.strip_prefix("#EXTINF:") {
            let dur: f64 = dur_str.trim_end_matches(',').parse().unwrap_or_else(|e| {
                tracing::warn!(
                    "failed to parse EXTINF duration '{dur_str}': {e}, defaulting to 10s"
                );
                10.0
            });
            pending_duration_ms = Some((dur * 1000.0) as u64);
        } else if line.ends_with(".m4s") && !line.starts_with('#') {
            let id = line
                .trim_start_matches("./")
                .trim_end_matches(".m4s")
                .to_string();
            // start_ts is the numeric component after the last ':'
            if let Some(ts_str) = id.rsplit(':').next() {
                if let Ok(start_ms) = ts_str.parse::<u64>() {
                    let duration_ms = pending_duration_ms.unwrap_or(10_000);
                    segments.push(CoverageSegment {
                        id,
                        start_ms,
                        end_ms: start_ms + duration_ms,
                    });
                }
            }
            pending_duration_ms = None;
        }
    }

    segments
}

#[derive(Serialize, Clone)]
pub struct CoverageSegment {
    pub id: String,
    pub start_ms: u64,
    pub end_ms: u64,
}

#[derive(Serialize)]
pub struct CoverageResponse {
    pub online: bool,
    pub segments: Vec<CoverageSegment>,
}

/// GET /hls/:device_id/playlist.m3u8
/// Serves from in-memory slot when camera is connected; falls back to Redis
/// so historical footage remains browseable while the camera is offline.
pub async fn get_manifest(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    // Try live slot first
    if let Some(slot) = state.registry.get_slot(&device_id).await {
        let manifest = slot.manifest.read().await;
        if let Some(m) = manifest.as_ref() {
            return manifest_response(normalize_manifest_for_browser(m));
        }
    }

    // Fall back to Redis for offline-camera playback
    if let Some(ref redis) = state.redis {
        if let Some(m) = crate::redis::manifest::get_manifest(redis, &device_id).await {
            return manifest_response(normalize_manifest_for_browser(&m));
        }
    }

    StatusCode::NOT_FOUND.into_response()
}

fn manifest_response(body: String) -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "application/vnd.apple.mpegurl")
        .header("cache-control", "no-cache")
        .body(axum::body::Body::from(body))
        .expect("manifest response")
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
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    // Check if init segment is already cached
    {
        let init = slot.init_segment.read().await;
        if let Some(data) = init.as_ref() {
            return init_response(data.clone());
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
            return init_response(data.clone());
        }
        if start.elapsed() > deadline {
            return StatusCode::GATEWAY_TIMEOUT.into_response();
        }
    }
}

fn init_response(data: Bytes) -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "video/mp4")
        .header("cache-control", "no-store, no-cache, must-revalidate")
        .body(axum::body::Body::from(data))
        .expect("init response")
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
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
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
                let (tx, rx) = oneshot::channel();
                waiters.push(tx);
                rx
            }
            Some(SegmentState::Buffered { data, .. }) => {
                return segment_response(data.clone());
            }
            None => {
                let (tx, rx) = oneshot::channel();
                segments.insert(
                    segment_id.clone(),
                    SegmentState::Uploading { waiters: vec![tx] },
                );

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
        .expect("segment response")
}

/// GET /hls/:device_id/coverage
/// Returns the segment time windows available for this camera.
/// Works whether the camera is online (reads from slot) or offline (reads from Redis).
pub async fn get_coverage(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id == user.user_id => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let online = state.registry.get_slot(&device_id).await.is_some();

    // Read manifest: live slot → Redis fallback
    let manifest_text = if let Some(slot) = state.registry.get_slot(&device_id).await {
        slot.manifest.read().await.clone()
    } else {
        None
    };

    let manifest_text = if manifest_text.is_some() {
        manifest_text
    } else if let Some(ref redis) = state.redis {
        crate::redis::manifest::get_manifest(redis, &device_id).await
    } else {
        None
    };

    let segments = manifest_text
        .as_deref()
        .map(parse_manifest_segments)
        .unwrap_or_default();

    axum::Json(CoverageResponse { online, segments }).into_response()
}
