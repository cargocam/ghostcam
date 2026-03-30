use std::path::Path;

use anyhow::Result;
use ghostcam::wire::alert::Alert;
use ghostcam::wire::framing;
use tokio::sync::Mutex;

/// Send a ClaimToken alert to the server on the control stream.
///
/// Called when the camera scans a claim QR code while in unclaimed mode.
pub async fn send_claim_token(alerts_tx: &Mutex<quinn::SendStream>, token: &str) -> Result<()> {
    let alert = Alert::ClaimToken {
        token: token.to_string(),
    };
    let mut stream = alerts_tx.lock().await;
    framing::write_json(&mut *stream, &alert)
        .await
        .map_err(|e| anyhow::anyhow!("claim token write error: {e}"))?;

    tracing::info!("claim token sent to server");
    Ok(())
}

/// Clear enrollment state (for unregistration).
pub async fn clear_enrollment(data_dir: &Path) -> Result<()> {
    // Remove legacy user cert files (no longer used, but clean up if present)
    let _ = tokio::fs::remove_file(data_dir.join("user.crt")).await;
    let _ = tokio::fs::remove_file(data_dir.join("user.key")).await;
    let _ = tokio::fs::remove_file(data_dir.join("server_fingerprint")).await;
    // Keep device.key and device.crt — device identity persists
    tracing::info!("enrollment state cleared");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn clear_enrollment_removes_files() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("user.crt"), "cert").unwrap();
        std::fs::write(dir.path().join("server_fingerprint"), "fp").unwrap();

        clear_enrollment(dir.path()).await.unwrap();

        assert!(!dir.path().join("user.crt").exists());
        assert!(!dir.path().join("server_fingerprint").exists());
    }
}
