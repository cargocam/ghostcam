use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::{Extension, Json};
use bytes::Bytes;
use ghostcam::types::DeviceId;
use ghostcam::wire::command::Command;
use serde::{Deserialize, Serialize};
use tokio::sync::oneshot;
use tokio::time::{timeout, Duration};

use super::auth::AuthUser;
use super::state::AppState;
use crate::ingest::slot::SegmentState;

/// Number of segments to pre-fetch ahead when a segment is requested on-demand.
const PREFETCH_LOOKAHEAD: usize = 3;

pub(crate) fn normalize_manifest_for_browser(manifest: &str) -> String {
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    // Try live slot first (pre-normalized cache avoids per-request processing)
    if let Some(slot) = state.registry.get_slot(&device_id).await {
        let normalized = slot.manifest_normalized.read().await;
        if let Some(m) = normalized.as_ref() {
            return manifest_response(m.clone());
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    // Register the waiter *before* the cache check so notify_waiters()
    // cannot fire in the gap between the check and the await.
    // enable() registers immediately without polling, so the waiter is
    // visible to notify_waiters() even before we reach the await point.
    let notified = slot.init_notify.notified();
    tokio::pin!(notified);
    notified.as_mut().enable();

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

    // Wait for init segment to arrive (event-driven, no polling)
    match timeout(Duration::from_secs(10), &mut notified).await {
        Ok(_) => {
            let init = slot.init_segment.read().await;
            match init.as_ref() {
                Some(data) => init_response(data.clone()),
                None => {
                    tracing::warn!(device_id = %device_id, "init_notify fired but init_segment is None");
                    StatusCode::INTERNAL_SERVER_ERROR.into_response()
                }
            }
        }
        Err(_) => StatusCode::GATEWAY_TIMEOUT.into_response(),
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    // Single write-lock handles all three cases: Buffered (return immediately),
    // Uploading (add waiter), None (insert Uploading and request upload from camera).
    // This avoids a read→write lock upgrade race where the state could change between locks.
    let (rx, needs_command) = {
        let mut segments = slot.segments.write().await;
        match segments.get_mut(&segment_id) {
            Some(SegmentState::Buffered { data, .. }) => {
                return segment_response(data.clone());
            }
            Some(SegmentState::Uploading { waiters }) => {
                let (tx, rx) = oneshot::channel();
                waiters.push(tx);
                (rx, false)
            }
            None => {
                let (tx, rx) = oneshot::channel();
                segments.insert(
                    segment_id.clone(),
                    SegmentState::Uploading { waiters: vec![tx] },
                );
                (rx, true)
            }
        }
    };

    // Send command outside the lock scope to avoid holding the lock across an await.
    if needs_command {
        let _ = slot
            .send_command(Command::UploadSegment {
                seq: slot.next_seq(),
                segment_id: segment_id.clone(),
            })
            .await;

        // Parallel pre-fetch: request the next N segments from the manifest so they
        // are already uploading by the time HLS.js asks for them.
        prefetch_next_segments(&slot, &segment_id).await;
    }

    match timeout(Duration::from_secs(60), rx).await {
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

/// Fire-and-forget: request upload of the next N segments after `current_segment_id`
/// from the manifest. This creates `Uploading` state entries so the actual get_segment
/// handler will coalesce onto the already-in-progress upload.
async fn prefetch_next_segments(
    slot: &Arc<crate::ingest::slot::IngestSlot>,
    current_segment_id: &str,
) {
    let manifest = slot.manifest_normalized.read().await.clone();
    let manifest = match manifest {
        Some(m) => m,
        None => return,
    };

    let all_segments = parse_manifest_segments(&manifest);
    let pos = all_segments.iter().position(|s| s.id == current_segment_id);
    let pos = match pos {
        Some(p) => p,
        None => return,
    };

    let lookahead =
        &all_segments[pos + 1..std::cmp::min(pos + 1 + PREFETCH_LOOKAHEAD, all_segments.len())];

    // Collect IDs to prefetch under the lock, then drop it before sending commands.
    let to_prefetch = {
        let mut segments = slot.segments.write().await;
        let mut ids = Vec::new();
        for seg in lookahead {
            if !segments.contains_key(&seg.id) {
                segments.insert(seg.id.clone(), SegmentState::Uploading { waiters: vec![] });
                ids.push(seg.id.clone());
            }
        }
        ids
    };

    for id in to_prefetch {
        let _ = slot
            .send_command(Command::UploadSegment {
                seq: slot.next_seq(),
                segment_id: id,
            })
            .await;
    }
}

#[derive(Deserialize)]
pub struct PrefetchRequest {
    pub from_ms: u64,
    pub to_ms: u64,
}

/// POST /hls/:device_id/prefetch
/// Hint endpoint: tells the server to pre-fetch segments covering a time range.
/// Returns 202 immediately — does not wait for uploads to complete.
pub async fn prefetch(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
    Json(body): Json<PrefetchRequest>,
) -> Response {
    let device_id = DeviceId(device_id);

    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = match state.registry.get_slot(&device_id).await {
        Some(s) => s,
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    // Read manifest and find segments overlapping the requested time range
    let manifest = slot.manifest_normalized.read().await.clone();
    let manifest = match manifest {
        Some(m) => m,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    let all_segments = parse_manifest_segments(&manifest);
    let matching_ids: Vec<String> = all_segments
        .iter()
        .filter(|s| s.start_ms < body.to_ms && s.end_ms > body.from_ms)
        .map(|s| s.id.clone())
        .collect();

    if matching_ids.is_empty() {
        return StatusCode::ACCEPTED.into_response();
    }

    // Collect IDs under the lock, then drop it before sending commands.
    let to_prefetch = {
        let mut segments = slot.segments.write().await;
        let mut ids = Vec::new();
        for id in &matching_ids {
            if !segments.contains_key(id) {
                segments.insert(id.clone(), SegmentState::Uploading { waiters: vec![] });
                ids.push(id.clone());
            }
        }
        ids
    };

    for id in &to_prefetch {
        let _ = slot
            .send_command(Command::UploadSegment {
                seq: slot.next_seq(),
                segment_id: id.clone(),
            })
            .await;
    }

    tracing::debug!(
        device_id = %device_id,
        from_ms = body.from_ms,
        to_ms = body.to_ms,
        total = matching_ids.len(),
        requested = to_prefetch.len(),
        "prefetch hint processed"
    );

    StatusCode::ACCEPTED.into_response()
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
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let slot = state.registry.get_slot(&device_id).await;
    let online = slot.is_some();

    // Read manifest: live slot (pre-normalized) → Redis fallback
    let manifest_text = if let Some(ref slot) = slot {
        slot.manifest_normalized.read().await.clone()
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
