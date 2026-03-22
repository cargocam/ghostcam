use std::convert::Infallible;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::State;
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::Extension;
use tokio_stream::wrappers::BroadcastStream;
use tokio_stream::StreamExt;

use super::auth::AuthUser;
use super::state::AppState;

/// GET /events
pub async fn handle_sse(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Sse<impl tokio_stream::Stream<Item = Result<Event, Infallible>>> {
    let rx = state.sse_bus.subscribe(&user.user_id).await;
    let stream = BroadcastStream::new(rx).filter_map(|event| match event {
        Ok(sse_event) => {
            let data = serde_json::to_string(&sse_event).ok()?;
            let event_type = match &sse_event {
                crate::sse::SseEvent::CameraOnline { .. } => "camera_online",
                crate::sse::SseEvent::CameraOffline { .. } => "camera_offline",
            };
            Some(Ok(Event::default().event(event_type).data(data)))
        }
        Err(_) => None,
    });

    Sse::new(stream).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)))
}
