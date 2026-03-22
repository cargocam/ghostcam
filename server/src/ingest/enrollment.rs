use anyhow::{Context, Result};
use ghostcam::types::{CertFingerprint, UserId};
use ghostcam::wire::alert::Alert;
use ghostcam::wire::command::Command;
use ghostcam::wire::framing;

use crate::db_trait::{Database, NewCameraRecord, SOLO_USER_ID};
use crate::frames::InboundStreamTag;
use crate::pki::ca::CaManager;

/// Handle an enrollment QUIC connection.
///
/// The camera connects with only a device cert (no user association cert).
/// The enrollment flow:
/// 1. Read enrollment alert with JWT from the bidi control stream
/// 2. Verify JWT signature and expiry
/// 3. Read CSR alert from the control stream
/// 4. Create camera record in database
/// 5. Sign the CSR with the CA
/// 6. Send cert_refresh command with signed cert + CA cert
/// 7. Wait for ack alert
/// 8. Claim the enrollment token in the database
pub async fn handle_enrollment(
    connection: quinn::Connection,
    fingerprint: CertFingerprint,
    ca: &CaManager,
    db: &dyn Database,
) -> Result<()> {
    // Accept the bidirectional control stream
    let (mut commands_tx, mut alerts_rx) = connection
        .accept_bi()
        .await
        .map_err(|e| anyhow::anyhow!("failed to accept control stream: {e}"))?;

    // Read the stream tag — must be Alerts
    let mut tag_buf = [0u8; 1];
    alerts_rx.read_exact(&mut tag_buf).await?;
    let tag = InboundStreamTag::try_from(tag_buf[0])?;
    if tag != InboundStreamTag::Alerts {
        anyhow::bail!(
            "expected Alerts stream tag (0x10), got 0x{:02x}",
            tag_buf[0]
        );
    }

    // 1. Read enrollment alert with JWT
    let enrollment_alert: Alert = framing::read_json(&mut alerts_rx)
        .await
        .map_err(|e| anyhow::anyhow!("failed to read enrollment alert: {e}"))?
        .ok_or_else(|| anyhow::anyhow!("stream closed before enrollment alert"))?;

    let token = match &enrollment_alert {
        Alert::Enrollment { token } => token.clone(),
        other => {
            anyhow::bail!("expected enrollment alert, got: {:?}", other);
        }
    };

    // 2. Verify JWT
    let claims = ca
        .verify_enrollment_jwt(&token)
        .context("enrollment JWT verification failed")?;

    // 3. Check jti hasn't been claimed yet (claim returns false if already used)
    // We'll claim it after successful enrollment (step 8)
    let jti = claims.jti.clone();

    // 4. Read CSR alert
    let csr_alert: Alert = framing::read_json(&mut alerts_rx)
        .await
        .map_err(|e| anyhow::anyhow!("failed to read CSR alert: {e}"))?
        .ok_or_else(|| anyhow::anyhow!("stream closed before CSR alert"))?;

    let csr_pem = match &csr_alert {
        Alert::Csr { csr_pem } => csr_pem.clone(),
        other => {
            anyhow::bail!("expected CSR alert, got: {:?}", other);
        }
    };

    // 5. Create camera record in database
    let display_name = claims.display_name.unwrap_or_else(|| "Camera".to_string());

    let camera = db
        .create_camera(&NewCameraRecord {
            user_id: UserId(SOLO_USER_ID.to_string()),
            cert_fingerprint: fingerprint.clone(),
            display_name,
        })
        .await
        .context("failed to create camera record")?;

    // 6. Sign the CSR
    let signed_cert_pem = ca
        .sign_csr(&csr_pem, &camera.device_id.0)
        .context("failed to sign CSR")?;

    // 7. Send cert_refresh command
    let cmd = Command::CertRefresh {
        seq: 0,
        cert_pem: signed_cert_pem,
        ca_pem: Some(ca.ca_cert_pem().to_string()),
    };
    framing::write_json(&mut commands_tx, &cmd)
        .await
        .map_err(|e| anyhow::anyhow!("failed to send cert_refresh: {e}"))?;

    // 8. Wait for ack alert (with timeout)
    let ack_result = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        framing::read_json::<Alert, _>(&mut alerts_rx),
    )
    .await;

    match ack_result {
        Ok(Ok(Some(Alert::Ack { cmd, .. }))) if cmd == "cert_refresh" => {
            // Claim the enrollment token
            let claimed = db
                .claim_enrollment_token(&jti, &camera.device_id)
                .await
                .context("failed to claim enrollment token")?;

            if !claimed {
                // Token was already claimed (race condition or replay)
                tracing::warn!(jti = %jti, "enrollment token already claimed");
                db.delete_camera(&camera.device_id).await?;
                connection.close(5u32.into(), b"enrollment token already claimed");
                return Err(anyhow::anyhow!("enrollment token already claimed"));
            }

            tracing::info!(
                device_id = %camera.device_id,
                fingerprint = %fingerprint.0,
                "camera enrolled successfully"
            );
        }
        Ok(Ok(Some(other))) => {
            // Unexpected alert — clean up
            db.delete_camera(&camera.device_id).await?;
            anyhow::bail!("expected ack alert, got: {:?}", other);
        }
        Ok(Ok(None)) => {
            // Stream closed
            db.delete_camera(&camera.device_id).await?;
            anyhow::bail!("stream closed before ack");
        }
        Ok(Err(e)) => {
            db.delete_camera(&camera.device_id).await?;
            anyhow::bail!("error reading ack: {e}");
        }
        Err(_) => {
            // Timeout
            db.delete_camera(&camera.device_id).await?;
            anyhow::bail!("timeout waiting for enrollment ack");
        }
    }

    connection.close(0u32.into(), b"enrollment complete");
    Ok(())
}
