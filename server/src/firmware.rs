use std::sync::Arc;

use crate::api::state::AppState;

/// Schedule staggered reboot commands to all connected cameras.
///
/// With QUIC ingest removed, cameras now poll for firmware updates on their own.
/// This is a no-op stub retained for call-site compatibility (GitHub webhook handler).
pub fn schedule_staggered_reboot(state: &Arc<AppState>) {
    let version = {
        let release = state.firmware_release.try_read();
        release
            .ok()
            .and_then(|r| r.as_ref().map(|r| r.version.clone()))
    };

    if let Some(version) = version {
        tracing::info!(
            version,
            "new firmware release recorded — cameras will pick it up on next check-in"
        );
    }
}
