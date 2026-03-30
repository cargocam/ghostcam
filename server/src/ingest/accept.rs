use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;

use anyhow::Result;
use ghostcam::config::QUIC_MAX_CONNECTIONS;
use ghostcam::firmware::FirmwareRelease;
use ghostcam::types::UserId;
use ghostcam::wire::alert::Alert;
use ghostcam::wire::command::{Command, DeviceStatusKind};
use ghostcam::wire::framing;
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;

use super::registry::RoutingRegistry;
use super::slot::IngestSlot;
use crate::audit::{AuditEvent, AuditLogger};
use crate::db_trait::Database;
use crate::frames::InboundStreamTag;
use crate::pki::ca::CaManager;
use crate::pki::revocation::RevocationCache;
use crate::redis::connection::RedisManager;
use crate::redis::telemetry::TelemetryBatcher;
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
    telemetry_batcher: Option<Arc<TelemetryBatcher>>,
    sse_bus: Arc<SseEventBus>,
    audit: Arc<AuditLogger>,
    firmware_release: Arc<RwLock<Option<FirmwareRelease>>>,
    cancel: CancellationToken,
) -> Result<()> {
    let active_connections = Arc::new(AtomicU32::new(0));

    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            incoming = endpoint.accept() => {
                let Some(incoming) = incoming else { break };

                let prev = active_connections.fetch_add(1, Ordering::Relaxed);
                if prev >= QUIC_MAX_CONNECTIONS {
                    active_connections.fetch_sub(1, Ordering::Relaxed);
                    tracing::warn!(current = prev, limit = QUIC_MAX_CONNECTIONS, "connection limit reached — refusing");
                    incoming.refuse();
                    continue;
                }

                let registry = registry.clone();
                let db = db.clone();
                let ca = ca.clone();
                let revocation_cache = revocation_cache.clone();
                let redis = redis.clone();
                let batcher = telemetry_batcher.clone();
                let sse_bus = sse_bus.clone();
                let audit = audit.clone();
                let fw_release = firmware_release.clone();
                let conn_count = active_connections.clone();
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(incoming, registry, db, ca, revocation_cache, redis, batcher, sse_bus, audit, fw_release).await {
                        tracing::warn!("connection failed: {e}");
                    }
                    conn_count.fetch_sub(1, Ordering::Relaxed);
                });
            }
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn handle_connection(
    incoming: quinn::Incoming,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
    ca: Arc<CaManager>,
    revocation_cache: Arc<RevocationCache>,
    redis: Option<Arc<RedisManager>>,
    telemetry_batcher: Option<Arc<TelemetryBatcher>>,
    sse_bus: Arc<SseEventBus>,
    audit: Arc<AuditLogger>,
    firmware_release: Arc<RwLock<Option<FirmwareRelease>>>,
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

    // 3. Check revocation
    if revocation_cache.is_revoked(&fingerprint.0).await {
        tracing::warn!(fingerprint = %fingerprint.0, "revoked fingerprint — rejecting");
        connection.close(3u32.into(), b"certificate revoked");
        return Ok(());
    }

    // 4. Look up device by fingerprint, or auto-register if unknown
    let camera = match db.get_camera_by_fingerprint(&fingerprint).await? {
        Some(cam) => cam,
        None => {
            // Auto-register: create unclaimed device record
            tracing::info!(fingerprint = %fingerprint.0, "new device — auto-registering as unclaimed");
            db.register_device(&fingerprint, "Camera").await?
        }
    };

    // 5. Update last_seen
    db.update_last_seen(&camera.device_id).await?;

    // 6. Accept the control bidirectional stream.
    let (mut commands_stream, mut alerts_stream) = connection
        .accept_bi()
        .await
        .map_err(|e| anyhow::anyhow!("failed to accept control stream: {e}"))?;

    // 7. Read the stream tag — must be Alerts
    let mut tag_buf = [0u8; 1];
    alerts_stream.read_exact(&mut tag_buf).await?;
    let tag = InboundStreamTag::try_from(tag_buf[0])?;
    if tag != InboundStreamTag::Alerts {
        anyhow::bail!(
            "expected Alerts stream tag (0x10), got 0x{:02x}",
            tag_buf[0]
        );
    }

    // 8. Read the handshake alert
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

    let peer_ip = connection.remote_address().ip().to_string();

    // 9. Determine device status and send DeviceStatus command
    let user_id = match &camera.user_id {
        Some(uid) => {
            // Device is claimed — send Active status
            tracing::info!(
                device_id = %camera.device_id,
                fw_version = %fw_version,
                "claimed camera connected"
            );

            let status_cmd = Command::DeviceStatus {
                seq: 0,
                status: DeviceStatusKind::Active,
            };
            framing::write_json(&mut commands_stream, &status_cmd)
                .await
                .map_err(|e| anyhow::anyhow!("failed to send DeviceStatus: {e}"))?;

            uid.clone()
        }
        None => {
            // Device is unclaimed — send Unclaimed status, wait for ClaimToken
            tracing::info!(
                device_id = %camera.device_id,
                fingerprint = %fingerprint.0,
                "unclaimed camera connected — waiting for claim"
            );

            let status_cmd = Command::DeviceStatus {
                seq: 0,
                status: DeviceStatusKind::Unclaimed,
            };
            framing::write_json(&mut commands_stream, &status_cmd)
                .await
                .map_err(|e| anyhow::anyhow!("failed to send DeviceStatus: {e}"))?;

            // Wait for ClaimToken alert (with timeout)
            let claim_result = wait_for_claim(
                &mut alerts_stream,
                &mut commands_stream,
                &camera.device_id,
                &ca,
                db.as_ref(),
                &audit,
            )
            .await?;

            claim_result
        }
    };

    audit.log(AuditEvent::CameraConnected {
        device_id: camera.device_id.0.clone(),
        ip: peer_ip,
        firmware_version: fw_version.clone(),
    });

    // 10. Create IngestSlot and register
    let (slot, supervisor) = IngestSlot::spawn(
        camera.device_id.clone(),
        user_id.clone(),
        connection,
        alerts_stream,
        commands_stream,
        redis,
        telemetry_batcher,
    )
    .await;

    *slot.capabilities.write().await = streams;

    registry.register(slot.clone()).await;

    // 10b. Check if camera firmware is stale and send Reboot if needed
    {
        let latest = firmware_release.read().await.clone();
        crate::firmware::check_and_reboot_if_stale(
            &registry,
            &camera.device_id,
            &fw_version,
            &latest,
        )
        .await;
    }

    // 11. Publish SSE camera_online event
    sse_bus
        .publish(
            &user_id,
            crate::sse::SseEvent::CameraOnline {
                device_id: camera.device_id.0.clone(),
            },
        )
        .await;

    // 12. Wait for the slot supervisor to finish (camera disconnect or error)
    let _ = supervisor.await;

    // 13. Unregister on disconnect (only if this is still the active slot)
    registry.unregister(&camera.device_id, &slot).await;

    // 14. Publish SSE camera_offline event
    sse_bus
        .publish(
            &user_id,
            crate::sse::SseEvent::CameraOffline {
                device_id: camera.device_id.0.clone(),
            },
        )
        .await;

    audit.log(AuditEvent::CameraDisconnected {
        device_id: camera.device_id.0.clone(),
        reason: "disconnected".to_string(),
    });

    tracing::info!(device_id = %camera.device_id, "camera disconnected");

    Ok(())
}

/// Wait for a ClaimToken alert from an unclaimed camera.
/// Validates the JWT, claims the device, and returns the owner's UserId.
async fn wait_for_claim(
    alerts_stream: &mut quinn::RecvStream,
    commands_stream: &mut quinn::SendStream,
    device_id: &ghostcam::types::DeviceId,
    ca: &CaManager,
    db: &dyn Database,
    audit: &AuditLogger,
) -> Result<UserId> {
    // Wait up to 10 minutes for a claim token (camera is in QR scan mode)
    let timeout = std::time::Duration::from_secs(600);

    let alert_result = tokio::time::timeout(timeout, async {
        loop {
            let alert: Alert = framing::read_json(alerts_stream)
                .await
                .map_err(|e| anyhow::anyhow!("failed to read alert while waiting for claim: {e}"))?
                .ok_or_else(|| anyhow::anyhow!("stream closed while waiting for claim"))?;

            match alert {
                Alert::ClaimToken { token } => return Ok(token),
                Alert::Ack { .. } => {
                    // Ignore acks
                    continue;
                }
                other => {
                    tracing::debug!(device_id = %device_id, "ignoring alert while waiting for claim: {:?}", other);
                    continue;
                }
            }
        }
    })
    .await;

    let token = match alert_result {
        Ok(Ok(t)) => t,
        Ok(Err(e)) => return Err(e),
        Err(_) => {
            anyhow::bail!("timeout waiting for claim token from device {}", device_id);
        }
    };

    // Verify the claim JWT
    let claims = ca
        .verify_enrollment_jwt(&token)
        .map_err(|e| anyhow::anyhow!("claim token verification failed: {e}"))?;

    let jti = claims.jti.clone();

    // Look up the user who created this enrollment token
    let user_id = db
        .get_enrollment_token_user_id(&jti)
        .await?
        .ok_or_else(|| anyhow::anyhow!("claim token has no associated user or is expired"))?;

    // Claim the enrollment token (mark it used)
    let claimed = db.claim_enrollment_token(&jti, device_id).await?;
    if !claimed {
        anyhow::bail!("claim token already used");
    }

    // Assign ownership
    db.claim_device(device_id, &user_id).await?;

    tracing::info!(
        device_id = %device_id,
        user_id = %user_id,
        "device claimed successfully"
    );

    audit.log(AuditEvent::EnrollmentCompleted {
        device_id: device_id.0.clone(),
        owner_id: user_id.0.clone(),
    });

    // Send Active status
    let status_cmd = Command::DeviceStatus {
        seq: 1,
        status: DeviceStatusKind::Active,
    };
    framing::write_json(commands_stream, &status_cmd)
        .await
        .map_err(|e| anyhow::anyhow!("failed to send Active status: {e}"))?;

    Ok(user_id)
}
