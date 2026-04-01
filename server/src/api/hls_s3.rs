use std::sync::Arc;

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use ghostcam::config::SEGMENT_DURATION_SECS;
use ghostcam::types::DeviceId;
use serde::{Deserialize, Serialize};

use super::auth::AuthUser;
use super::state::AppState;
use crate::s3::S3Client;

#[derive(Deserialize)]
pub struct ManifestQuery {
    /// Start time (Unix ms). Defaults to last 5 minutes for live-ish.
    pub from: Option<u64>,
    /// End time (Unix ms). Defaults to now.
    pub to: Option<u64>,
}

/// GET /hls/:device_id/playlist.m3u8?from=&to=
///
/// Generates an HLS manifest on the fly from the segments table.
/// Each segment URI is a presigned S3 GET URL (served directly from Tigris).
pub async fn get_manifest(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
    Query(query): Query<ManifestQuery>,
) -> Response {
    let device_id = DeviceId(device_id);

    // Verify ownership
    match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => {}
        _ => return StatusCode::NOT_FOUND.into_response(),
    }

    let s3 = match state.s3.as_ref() {
        Some(s3) => s3,
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    let now_ms = now_ms();
    let to_ms = query.to.unwrap_or(now_ms);
    // Default window: last 5 minutes for live-ish viewing
    let from_ms = query.from.unwrap_or_else(|| to_ms.saturating_sub(5 * 60 * 1000));

    let segments = match state
        .db
        .list_segments(&device_id, from_ms, to_ms)
        .await
    {
        Ok(segs) => segs,
        Err(e) => {
            tracing::error!(device_id = %device_id, "list segments failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    if segments.is_empty() {
        return StatusCode::NOT_FOUND.into_response();
    }

    // Build HLS manifest
    let mut m3u8 = String::with_capacity(segments.len() * 200);
    m3u8.push_str("#EXTM3U\n");
    m3u8.push_str("#EXT-X-VERSION:7\n");
    m3u8.push_str(&format!(
        "#EXT-X-TARGETDURATION:{}\n",
        SEGMENT_DURATION_SECS
    ));
    m3u8.push_str("#EXT-X-MEDIA-SEQUENCE:0\n");
    m3u8.push_str("#EXT-X-PLAYLIST-TYPE:EVENT\n");

    // Init segment (presigned GET URL)
    let init_key = S3Client::init_key(&device_id.0);
    match s3.presign_get(&init_key).await {
        Ok(init_url) => {
            m3u8.push_str(&format!("#EXT-X-MAP:URI=\"{init_url}\"\n"));
        }
        Err(e) => {
            tracing::warn!(device_id = %device_id, "presign init GET failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // Media segments with wall-clock timestamps for playback seeking
    for seg in &segments {
        let duration_secs = (seg.end_ts - seg.start_ts) as f64 / 1000.0;
        match s3.presign_get(&seg.s3_key).await {
            Ok(seg_url) => {
                // #EXT-X-PROGRAM-DATE-TIME lets hls.js map video time to wall-clock time
                let dt = epoch_ms_to_iso8601(seg.start_ts);
                m3u8.push_str(&format!("#EXT-X-PROGRAM-DATE-TIME:{dt}\n"));
                m3u8.push_str(&format!("#EXTINF:{duration_secs:.3},\n"));
                m3u8.push_str(&seg_url);
                m3u8.push('\n');
            }
            Err(e) => {
                tracing::warn!(segment_id = %seg.segment_id, "presign GET failed: {e}");
                continue;
            }
        }
    }

    // If viewing live (no explicit `to` and recent segments exist), don't end the playlist
    // so hls.js keeps polling for new segments
    let last_segment_end = segments.last().map(|s| s.end_ts).unwrap_or(0);
    let is_live = query.to.is_none() && (now_ms - last_segment_end) < 30_000;
    if !is_live {
        m3u8.push_str("#EXT-X-ENDLIST\n");
    }

    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "application/vnd.apple.mpegurl")
        .header("cache-control", "no-cache")
        .body(axum::body::Body::from(m3u8))
        .expect("manifest response")
}

/// GET /hls/:device_id/init.mp4
///
/// Redirects to a presigned S3 GET URL for the init segment.
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

    let s3 = match state.s3.as_ref() {
        Some(s3) => s3,
        None => return StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };

    let init_key = S3Client::init_key(&device_id.0);
    match s3.presign_get(&init_key).await {
        Ok(url) => Response::builder()
            .status(StatusCode::TEMPORARY_REDIRECT)
            .header("location", url)
            .header("cache-control", "private, max-age=3600")
            .body(axum::body::Body::empty())
            .expect("init redirect"),
        Err(e) => {
            tracing::error!(device_id = %device_id, "presign init GET failed: {e}");
            StatusCode::NOT_FOUND.into_response()
        }
    }
}

#[derive(Serialize)]
pub struct CoverageSegment {
    pub id: String,
    pub start_ms: u64,
    pub end_ms: u64,
}

#[derive(Serialize)]
pub struct CoverageResponse {
    pub segments: Vec<CoverageSegment>,
}

/// GET /hls/:device_id/coverage
///
/// Returns time windows of available footage from the segments table.
pub async fn get_coverage(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Path(device_id): Path<String>,
) -> Response {
    let device_id = DeviceId(device_id);

    let camera = match state.db.get_camera(&device_id).await {
        Ok(Some(c)) if c.user_id.as_ref() == Some(&user.user_id) => c,
        _ => return StatusCode::NOT_FOUND.into_response(),
    };

    // Default: last 24 hours of coverage
    let now_ms = now_ms();
    let from_ms = now_ms.saturating_sub(24 * 60 * 60 * 1000);

    let segments = match state.db.list_segments(&device_id, from_ms, now_ms).await {
        Ok(segs) => segs,
        Err(e) => {
            tracing::error!(device_id = %device_id, "list segments failed: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    let coverage: Vec<CoverageSegment> = segments
        .into_iter()
        .map(|s| CoverageSegment {
            id: s.segment_id,
            start_ms: s.start_ts,
            end_ms: s.end_ts,
        })
        .collect();

    axum::Json(CoverageResponse {
        segments: coverage,
    })
    .into_response()
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64
}

fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

/// Convert epoch milliseconds to ISO 8601 for #EXT-X-PROGRAM-DATE-TIME.
fn epoch_ms_to_iso8601(epoch_ms: u64) -> String {
    let secs = (epoch_ms / 1000) as i64;
    let millis = (epoch_ms % 1000) as u32;
    let dt = chrono::DateTime::from_timestamp(secs, millis * 1_000_000)
        .unwrap_or_default();
    dt.format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string()
}
