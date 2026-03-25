use std::sync::Arc;

use anyhow::Result;
use ghostcam::wire::alert::Alert;
use ghostcam::wire::framing;
use tokio_util::sync::CancellationToken;

use super::enrollment::handle_enrollment;
use super::registry::RoutingRegistry;
use super::slot::IngestSlot;
use crate::db_trait::Database;
use crate::frames::InboundStreamTag;
use crate::pki::ca::CaManager;
use crate::pki::revocation::RevocationCache;
use crate::redis::connection::RedisManager;
use crate::sse::SseEventBus;

/// Run the QUIC accept loop, spawning a handler task per connection.
#[allow(clippy::too_many_arguments)]
pub async fn run_accept_loop(
    endpoint: quinn::Endpoint,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
    ca: Arc<CaManager>,
    revocation_cache: Arc<RevocationCache>,
    redis: Option<Arc<RedisManager>>,
    sse_bus: Arc<SseEventBus>,
    cancel: CancellationToken,
) -> Result<()> {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            incoming = endpoint.accept() => {
                let Some(incoming) = incoming else { break };
                let registry = registry.clone();
                let db = db.clone();
                let ca = ca.clone();
                let revocation_cache = revocation_cache.clone();
                let redis = redis.clone();
                let sse_bus = sse_bus.clone();
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(incoming, registry, db, ca, revocation_cache, redis, sse_bus).await {
                        tracing::warn!("connection failed: {e}");
                    }
                });
            }
        }
    }
    Ok(())
}

async fn handle_connection(
    incoming: quinn::Incoming,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
    ca: Arc<CaManager>,
    revocation_cache: Arc<RevocationCache>,
    redis: Option<Arc<RedisManager>>,
    sse_bus: Arc<SseEventBus>,
) -> Result<()> {
    let connection = incoming.await?;

    // 1. Extract client certificate from the TLS session
    let peer_certs = connection
        .peer_identity()
        .and_then(|id| id.downcast::<Vec<rustls::pki_types::CertificateDer>>().ok())
        .ok_or_else(|| anyhow::anyhow!("no client certificate"))?;

    if peer_certs.is_empty() {
        anyhow::bail!("empty client certificate chain");
    }

    // 2. Compute fingerprint of the device cert (first cert in chain)
    let fingerprint = ghostcam::pki::cert_fingerprint(&peer_certs[0]);

    // 3. Check for user association cert (second cert in chain)
    let has_association_cert = peer_certs.len() >= 2;

    if !has_association_cert {
        // Enrollment path
        tracing::info!(fingerprint = %fingerprint.0, "enrollment connection");
        return handle_enrollment(connection, fingerprint, &ca, db.as_ref()).await;
    }

    // Normal path — verify user association cert
    let user_cert_der = &peer_certs[1];

    // 4. Check revocation cache (using cert fingerprint as key)
    let user_cert_serial = ghostcam::pki::cert_serial_hex(user_cert_der).unwrap_or_default();
    if revocation_cache.is_revoked(&user_cert_serial).await {
        tracing::warn!(fingerprint = %fingerprint.0, serial = %user_cert_serial, "revoked certificate — rejecting");
        connection.close(3u32.into(), b"certificate revoked");
        return Ok(());
    }

    // 5. Verify user association cert was signed by our CA
    if let Err(e) = ca.verify_user_cert(user_cert_der) {
        tracing::warn!(fingerprint = %fingerprint.0, "user cert verification failed: {e}");
        connection.close(4u32.into(), b"invalid user certificate");
        return Ok(());
    }

    // 6. Extract device_id from user association cert CN
    let device_id_from_cert = ghostcam::pki::extract_cn(user_cert_der)?;

    // 7. Database lookup by fingerprint
    let camera = db
        .get_camera_by_fingerprint(&fingerprint)
        .await?
        .ok_or_else(|| anyhow::anyhow!("device not enrolled"))?;

    // 8. Verify device_id from cert matches database record
    if camera.device_id.0 != device_id_from_cert {
        tracing::warn!(
            fingerprint = %fingerprint.0,
            cert_device_id = %device_id_from_cert,
            db_device_id = %camera.device_id,
            "device identity mismatch"
        );
        connection.close(4u32.into(), b"device identity mismatch");
        return Err(anyhow::anyhow!("device_id mismatch"));
    }

    // 9. Check revocation by fingerprint too
    if revocation_cache.is_revoked(&fingerprint.0).await {
        tracing::warn!(fingerprint = %fingerprint.0, "revoked fingerprint — rejecting");
        connection.close(3u32.into(), b"certificate revoked");
        return Ok(());
    }

    // 10. Update last_seen
    db.update_last_seen(&camera.device_id).await?;

    // 11. Accept the control bidirectional stream.
    let (commands_stream, mut alerts_stream) = connection
        .accept_bi()
        .await
        .map_err(|e| anyhow::anyhow!("failed to accept control stream: {e}"))?;

    // 12. Read the stream tag — must be Alerts
    let mut tag_buf = [0u8; 1];
    alerts_stream.read_exact(&mut tag_buf).await?;
    let tag = InboundStreamTag::try_from(tag_buf[0])?;
    if tag != InboundStreamTag::Alerts {
        anyhow::bail!(
            "expected Alerts stream tag (0x10), got 0x{:02x}",
            tag_buf[0]
        );
    }

    // 13. Read the handshake alert
    let handshake: Alert = framing::read_json(&mut alerts_stream)
        .await
        .map_err(|e| anyhow::anyhow!("failed to read handshake: {e}"))?
        .ok_or_else(|| anyhow::anyhow!("alerts stream closed before handshake"))?;

    let (protocol_version, fw_version, streams) = match &handshake {
        Alert::Handshake {
            protocol_version,
            fw_version,
            streams,
        } => (*protocol_version, fw_version.clone(), streams.clone()),
        other => {
            anyhow::bail!("expected handshake alert, got: {:?}", other);
        }
    };

    if protocol_version != ghostcam::config::PROTOCOL_VERSION {
        connection.close(2u32.into(), b"unsupported protocol version");
        return Err(anyhow::anyhow!("unsupported protocol version"));
    }

    tracing::info!(
        device_id = %camera.device_id,
        fw_version = %fw_version,
        "camera connected"
    );

    // 14. Create IngestSlot and register
    let (slot, supervisor) = IngestSlot::spawn(
        camera.device_id.clone(),
        camera.user_id.clone(),
        connection,
        alerts_stream,
        commands_stream,
        redis,
    )
    .await;

    *slot.capabilities.write().await = streams;

    registry.register(slot.clone()).await;

    // 15. Publish SSE camera_online event
    sse_bus
        .publish(
            &camera.user_id,
            crate::sse::SseEvent::CameraOnline {
                device_id: camera.device_id.0.clone(),
            },
        )
        .await;

    // 16. Wait for the slot supervisor to finish (camera disconnect or error)
    let _ = supervisor.await;

    // 17. Unregister on disconnect (only if this is still the active slot)
    registry.unregister(&camera.device_id, &slot).await;

    // 18. Publish SSE camera_offline event
    sse_bus
        .publish(
            &camera.user_id,
            crate::sse::SseEvent::CameraOffline {
                device_id: camera.device_id.0.clone(),
            },
        )
        .await;

    tracing::info!(device_id = %camera.device_id, "camera disconnected");

    Ok(())
}
