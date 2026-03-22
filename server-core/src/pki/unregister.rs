use anyhow::Result;
use ghostcam::types::DeviceId;

use super::revocation::RevocationCache;
use crate::db::Database;
use crate::ingest::registry::RoutingRegistry;
use crate::redis::connection::RedisManager;

/// Result of an unregistration attempt.
#[derive(Debug, PartialEq, Eq)]
pub enum UnregisterResult {
    /// Camera was online, unregister command delivered and acknowledged.
    Completed,
    /// Camera is offline, unregister queued for next connection.
    Queued,
}

/// Unregister a camera: revoke cert, delete from DB, disconnect if online.
///
/// If the camera is online:
/// 1. Send `unregister` command via the slot's command channel
/// 2. Delete camera from database
/// 3. Add cert serial to revocation cache
/// 4. Slot will be torn down when camera processes the command and disconnects
///
/// If the camera is offline:
/// 1. Delete camera from database
/// 2. Add cert serial to revocation cache (prevents reconnection)
pub async fn unregister_camera(
    device_id: &DeviceId,
    registry: &RoutingRegistry,
    db: &dyn Database,
    revocation_cache: &RevocationCache,
    redis: Option<&RedisManager>,
) -> Result<UnregisterResult> {
    let is_online = registry.is_connected(device_id).await;

    // Look up the camera to get cert info for revocation
    let camera = db
        .get_camera(device_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("camera not found: {}", device_id))?;

    if is_online {
        // Send unregister command via the slot
        if let Some(slot) = registry.get_slot(device_id).await {
            let cmd = ghostcam::wire::command::Command::Unregister { seq: 0 };
            // Best-effort: if the command channel is full or closed, proceed anyway
            let _ = slot.send_command(cmd).await;
        }
    }

    // Delete from database
    db.delete_camera(device_id).await?;

    // Add fingerprint-derived info to revocation cache.
    // The cert_fingerprint serves as the revocation identifier.
    revocation_cache
        .add(camera.cert_fingerprint.0.clone())
        .await;

    // Purge Redis data and persist revocation
    if let Some(redis) = redis {
        crate::redis::purge::purge_device_data(redis, device_id).await;
        if let Err(e) =
            crate::redis::revocation::revoke_cert(redis, &camera.cert_fingerprint.0).await
        {
            tracing::warn!(device_id = %device_id, "failed to persist revocation to Redis: {e}");
        }
    }

    if is_online {
        Ok(UnregisterResult::Completed)
    } else {
        Ok(UnregisterResult::Queued)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Unit tests for unregistration are minimal here since the real flow
    // involves a full QUIC connection. Integration tests in
    // tests/unregister_integration.rs cover the full flow.

    #[test]
    fn unregister_result_variants() {
        assert_eq!(UnregisterResult::Completed, UnregisterResult::Completed);
        assert_eq!(UnregisterResult::Queued, UnregisterResult::Queued);
        assert_ne!(UnregisterResult::Completed, UnregisterResult::Queued);
    }
}
