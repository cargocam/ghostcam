use std::sync::Arc;
use std::time::Duration;

use axum::extract::State;
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::IntoResponse;
use axum::Extension;
use tokio_stream::wrappers::ReceiverStream;

use super::auth::AuthUser;
use super::state::AppState;
use crate::redis::telemetry::{fields_to_entry, TelemetryEntry};

/// GET /events
///
/// SSE stream that tails Redis telemetry streams for all of the authenticated
/// user's cameras. Pushes `telemetry` events as they arrive in realtime.
pub async fn handle_sse(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> impl IntoResponse {
    let (tx, rx) = tokio::sync::mpsc::channel::<Result<Event, std::convert::Infallible>>(64);

    tokio::spawn(sse_task(state, user, tx));

    Sse::new(ReceiverStream::new(rx)).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)))
}

/// Background task: XREAD BLOCK on all user's camera telemetry streams.
async fn sse_task(
    state: Arc<AppState>,
    user: AuthUser,
    tx: tokio::sync::mpsc::Sender<Result<Event, std::convert::Infallible>>,
) {
    // Get user's cameras
    let cameras = match state.db.list_cameras(&user.user_id).await {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!(user_id = %user.user_id, "SSE: failed to list cameras: {e}");
            return;
        }
    };

    if cameras.is_empty() {
        // No cameras — keep connection alive but nothing to stream
        let _ = tx.closed().await;
        return;
    }

    let redis = match state.redis.as_ref() {
        Some(r) => r,
        None => {
            tracing::debug!("SSE: redis not configured, no telemetry streaming");
            let _ = tx.closed().await;
            return;
        }
    };

    let mut conn = match redis.get_conn() {
        Some(c) => c,
        None => {
            tracing::debug!("SSE: redis not connected");
            return;
        }
    };

    // Build stream keys and initial IDs (start from latest — "$")
    let stream_keys: Vec<String> = cameras
        .iter()
        .map(|c| format!("telemetry:{}", c.device_id.0))
        .collect();
    let mut last_ids: Vec<String> = vec!["$".to_string(); stream_keys.len()];

    // Map stream key -> device_id for event tagging
    let key_to_device: std::collections::HashMap<String, String> = cameras
        .iter()
        .map(|c| (format!("telemetry:{}", c.device_id.0), c.device_id.0.clone()))
        .collect();

    loop {
        if tx.is_closed() {
            break;
        }

        // XREAD BLOCK 5000 STREAMS key1 key2 ... id1 id2 ...
        let mut cmd = redis::cmd("XREAD");
        cmd.arg("BLOCK").arg(5000u64).arg("STREAMS");
        for key in &stream_keys {
            cmd.arg(key);
        }
        for id in &last_ids {
            cmd.arg(id);
        }

        let result: Result<Option<Vec<StreamReadResult>>, redis::RedisError> =
            cmd.query_async(&mut conn).await;

        match result {
            Ok(Some(streams)) => {
                for stream in streams {
                    let device_id = match key_to_device.get(&stream.key) {
                        Some(id) => id,
                        None => continue,
                    };

                    // Update the last ID for this stream
                    if let Some(idx) = stream_keys.iter().position(|k| k == &stream.key) {
                        if let Some(last) = stream.entries.last() {
                            last_ids[idx] = last.id.clone();
                        }
                    }

                    // Send each entry as an SSE event
                    for entry in &stream.entries {
                        let telemetry = match fields_to_entry(&entry.fields) {
                            Ok(t) => t,
                            Err(_) => continue,
                        };

                        let payload = TelemetryEvent {
                            device_id: device_id.clone(),
                            telemetry,
                        };

                        let json = match serde_json::to_string(&payload) {
                            Ok(j) => j,
                            Err(_) => continue,
                        };

                        let event = Event::default().event("telemetry").data(json);
                        if tx.send(Ok(event)).await.is_err() {
                            return; // Client disconnected
                        }
                    }
                }
            }
            Ok(None) => {
                // Timeout, no new data — loop and block again
            }
            Err(e) => {
                tracing::debug!(user_id = %user.user_id, "SSE XREAD error: {e}");
                // Brief backoff before retry
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
}

#[derive(serde::Serialize)]
struct TelemetryEvent {
    device_id: String,
    telemetry: TelemetryEntry,
}

/// Parsed result from XREAD — one per stream that had data.
struct StreamReadResult {
    key: String,
    entries: Vec<StreamEntry>,
}

struct StreamEntry {
    id: String,
    fields: Vec<(String, String)>,
}

/// Parse the raw Redis XREAD response into our types.
/// XREAD returns: [ [key, [[id, [field, value, ...]], ...]], ... ]
impl redis::FromRedisValue for StreamReadResult {
    fn from_redis_value(v: &redis::Value) -> redis::RedisResult<Self> {
        // Each stream result is [key, entries]
        let items: Vec<redis::Value> = redis::FromRedisValue::from_redis_value(v)?;
        if items.len() != 2 {
            return Err(redis::RedisError::from((
                redis::ErrorKind::TypeError,
                "expected [key, entries]",
            )));
        }

        let key: String = redis::FromRedisValue::from_redis_value(&items[0])?;
        let raw_entries: Vec<redis::Value> = redis::FromRedisValue::from_redis_value(&items[1])?;

        let mut entries = Vec::new();
        for raw_entry in raw_entries {
            let pair: Vec<redis::Value> = redis::FromRedisValue::from_redis_value(&raw_entry)?;
            if pair.len() != 2 {
                continue;
            }
            let id: String = redis::FromRedisValue::from_redis_value(&pair[0])?;
            let flat_fields: Vec<String> = redis::FromRedisValue::from_redis_value(&pair[1])?;

            let mut fields = Vec::new();
            for chunk in flat_fields.chunks(2) {
                if chunk.len() == 2 {
                    fields.push((chunk[0].clone(), chunk[1].clone()));
                }
            }

            entries.push(StreamEntry { id, fields });
        }

        Ok(Self { key, entries })
    }
}
