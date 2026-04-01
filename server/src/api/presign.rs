use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use ghostcam::api_types::{PresignRequest, PresignResponse, PresignedUrl, UploadedSegment};
use ghostcam::config::PRESIGN_BATCH_MAX;

use super::auth::AuthCamera;
use super::state::AppState;
use crate::db_trait::SegmentRecord;
use crate::s3::S3Client;

/// POST /api/v1/cameras/:id/presign
///
/// Camera-auth endpoint. Serves two purposes:
/// 1. Confirm previously uploaded segments (moves them to "available" in DB)
/// 2. Return a batch of presigned PUT URLs for upcoming segment uploads
pub async fn presign(
    State(state): State<Arc<AppState>>,
    Extension(camera): Extension<AuthCamera>,
    Json(body): Json<PresignRequest>,
) -> Response {
    let s3 = match state.s3.as_ref() {
        Some(s3) => s3,
        None => {
            return (StatusCode::SERVICE_UNAVAILABLE, "S3 not configured").into_response();
        }
    };

    let device_id = &camera.device_id;

    // 1. Confirm uploaded segments
    if !body.uploaded.is_empty() {
        let records: Vec<SegmentRecord> = body
            .uploaded
            .iter()
            .map(|u| segment_record_from_upload(device_id.0.as_str(), u, s3))
            .collect();

        if let Err(e) = state.db.insert_segments(&records).await {
            tracing::error!(device_id = %device_id, "failed to confirm segments: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    }

    // 2. Generate presigned PUT URLs
    let count = body.count.min(PRESIGN_BATCH_MAX);
    let mut urls = Vec::with_capacity(count as usize);
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();

    for _ in 0..count {
        let segment_id = uuid::Uuid::new_v4().to_string();
        let s3_key = S3Client::segment_key(&device_id.0, &segment_id);

        match s3.presign_put(&s3_key).await {
            Ok(put_url) => {
                urls.push(PresignedUrl {
                    segment_id,
                    s3_key,
                    put_url,
                    expires_at: now + s3.presign_ttl_secs(),
                });
            }
            Err(e) => {
                tracing::error!(device_id = %device_id, "presign PUT failed: {e}");
                return StatusCode::INTERNAL_SERVER_ERROR.into_response();
            }
        }
    }

    // 3. Check if init segment needs uploading (first request from this camera)
    let init_url = match state.db.latest_segment(device_id).await {
        Ok(None) => {
            // No segments yet — camera probably needs to upload init segment
            let init_key = S3Client::init_key(&device_id.0);
            match s3.presign_put(&init_key).await {
                Ok(put_url) => Some(PresignedUrl {
                    segment_id: "init".to_string(),
                    s3_key: init_key,
                    put_url,
                    expires_at: now + s3.presign_ttl_secs(),
                }),
                Err(e) => {
                    tracing::warn!(device_id = %device_id, "presign init PUT failed: {e}");
                    None
                }
            }
        }
        _ => None,
    };

    Json(PresignResponse { urls, init_url }).into_response()
}

fn segment_record_from_upload(device_id: &str, u: &UploadedSegment, s3: &S3Client) -> SegmentRecord {
    let _ = s3; // key is computed statically
    SegmentRecord {
        segment_id: u.segment_id.clone(),
        device_id: ghostcam::types::DeviceId(device_id.to_string()),
        s3_key: S3Client::segment_key(device_id, &u.segment_id),
        start_ts: u.start_ts,
        end_ts: u.end_ts,
        size_bytes: u.size_bytes,
        resolution: String::new(), // camera's configured resolution; filled by caller if needed
        created_at: std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs(),
    }
}
