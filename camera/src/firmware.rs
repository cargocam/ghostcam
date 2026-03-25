use std::path::Path;

use anyhow::{Context, Result};
use ghostcam::wire::alert::Alert;

/// Handle a firmware update command.
///
/// Downloads the binary, verifies SHA-256, performs atomic swap, then exits
/// so the watchdog can restart with the new binary.
pub async fn handle_firmware_update(
    url: &str,
    expected_sha256: &str,
    version: &str,
    data_dir: &Path,
    alerts_tx: &tokio::sync::Mutex<quinn::SendStream>,
) -> Result<()> {
    tracing::info!(version, url, "firmware update — downloading");

    // Send UpdateApplying alert
    let applying = Alert::UpdateApplying {
        version: version.to_string(),
    };
    {
        let mut stream = alerts_tx.lock().await;
        let _ = ghostcam::wire::framing::write_json(&mut *stream, &applying).await;
    }

    let firmware_dir = data_dir.join("firmware");
    tokio::fs::create_dir_all(&firmware_dir)
        .await
        .context("creating firmware directory")?;

    let temp_path = firmware_dir.join("downloading");
    let current_path = firmware_dir.join("current");
    let previous_path = firmware_dir.join("previous");

    // Download
    match download_and_verify(url, expected_sha256, &temp_path).await {
        Ok(()) => {}
        Err(e) => {
            tracing::error!("firmware download/verify failed: {e}");
            let failed = Alert::UpdateFailed {
                version_attempted: version.to_string(),
                version_current: env!("CARGO_PKG_VERSION").to_string(),
                reason: if e.to_string().contains("hash mismatch") {
                    ghostcam::wire::alert::UpdateFailReason::HashMismatch
                } else {
                    ghostcam::wire::alert::UpdateFailReason::DownloadFailed
                },
            };
            let mut stream = alerts_tx.lock().await;
            let _ = ghostcam::wire::framing::write_json(&mut *stream, &failed).await;
            return Err(e);
        }
    }

    // Set executable (unix)
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::set_permissions(&temp_path, std::fs::Permissions::from_mode(0o755))
            .await
            .context("setting firmware executable permission")?;
    }

    // Atomic swap: current → previous, temp → current
    if current_path.exists() {
        if let Err(e) = tokio::fs::rename(&current_path, &previous_path).await {
            tracing::warn!("failed to preserve previous firmware: {e}");
        }
    }
    tokio::fs::rename(&temp_path, &current_path)
        .await
        .context("installing new firmware")?;

    // Remove health sentinel so watchdog can detect unhealthy start
    let sentinel = firmware_dir.join("healthy");
    let _ = tokio::fs::remove_file(&sentinel).await;

    tracing::info!(version, "firmware installed — exiting for restart");

    // Send success alert before exiting
    let success = Alert::UpdateSucceeded {
        version: version.to_string(),
    };
    {
        let mut stream = alerts_tx.lock().await;
        let _ = ghostcam::wire::framing::write_json(&mut *stream, &success).await;
    }

    // Exit cleanly — watchdog/systemd will restart with new binary
    std::process::exit(0);
}

/// Download a file and verify its SHA-256 hash.
async fn download_and_verify(url: &str, expected_sha256: &str, dest: &Path) -> Result<()> {
    // Use a simple HTTP GET via tokio (avoid adding reqwest dep)
    // For now, support file:// URLs for testing, and shell out to curl for http(s)
    let bytes = if url.starts_with("file://") {
        let raw = url.strip_prefix("file://").unwrap();
        let resolved = std::fs::canonicalize(raw).context("resolving firmware file path")?;
        tokio::fs::read(resolved)
            .await
            .context("reading firmware file")?
    } else {
        let output = tokio::process::Command::new("curl")
            .args(["-sfL", "--max-time", "300", url])
            .output()
            .await
            .context("running curl for firmware download")?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            anyhow::bail!("firmware download failed: {stderr}");
        }
        output.stdout
    };

    // Verify SHA-256
    let actual = ghostcam::pki::sha256_hex(&bytes);
    if actual != expected_sha256 {
        anyhow::bail!("firmware hash mismatch: expected {expected_sha256}, got {actual}");
    }

    tokio::fs::write(dest, &bytes)
        .await
        .context("writing firmware to temp file")?;

    tracing::info!(
        size = bytes.len(),
        sha256 = %actual,
        "firmware downloaded and verified"
    );

    Ok(())
}

/// Write health sentinel to indicate camera started successfully.
pub async fn mark_healthy(data_dir: &Path) {
    let sentinel = data_dir.join("firmware/healthy");
    if let Some(parent) = sentinel.parent() {
        let _ = tokio::fs::create_dir_all(parent).await;
    }
    let _ = tokio::fs::write(&sentinel, "ok").await;
    tracing::debug!("health sentinel written");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn mark_healthy_creates_file() {
        let dir = tempfile::tempdir().unwrap();
        mark_healthy(dir.path()).await;
        assert!(dir.path().join("firmware/healthy").exists());
    }

    #[tokio::test]
    async fn download_and_verify_file_url() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("firmware.bin");
        let content = b"test firmware binary content";
        std::fs::write(&src, content).unwrap();

        let hash = ghostcam::pki::sha256_hex(content);
        let dest = dir.path().join("downloaded");
        let url = format!("file://{}", src.display());

        download_and_verify(&url, &hash, &dest).await.unwrap();
        assert!(dest.exists());
        assert_eq!(std::fs::read(&dest).unwrap(), content);
    }

    #[tokio::test]
    async fn download_and_verify_wrong_hash() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("firmware.bin");
        std::fs::write(&src, b"content").unwrap();

        let dest = dir.path().join("downloaded");
        let url = format!("file://{}", src.display());

        let result = download_and_verify(&url, "badhash", &dest).await;
        assert!(result.is_err());
        assert!(result.unwrap_err().to_string().contains("hash mismatch"));
    }

    #[tokio::test]
    async fn atomic_swap_files() {
        let dir = tempfile::tempdir().unwrap();
        let fw_dir = dir.path().join("firmware");
        std::fs::create_dir_all(&fw_dir).unwrap();

        let current = fw_dir.join("current");
        let previous = fw_dir.join("previous");
        let temp = fw_dir.join("downloading");

        // Simulate existing firmware
        std::fs::write(&current, b"old-binary").unwrap();
        std::fs::write(&temp, b"new-binary").unwrap();

        // Perform swap
        if current.exists() {
            std::fs::rename(&current, &previous).unwrap();
        }
        std::fs::rename(&temp, &current).unwrap();

        assert_eq!(std::fs::read(&current).unwrap(), b"new-binary");
        assert_eq!(std::fs::read(&previous).unwrap(), b"old-binary");
        assert!(!temp.exists());
    }

    #[tokio::test]
    async fn first_update_no_previous() {
        let dir = tempfile::tempdir().unwrap();
        let fw_dir = dir.path().join("firmware");
        std::fs::create_dir_all(&fw_dir).unwrap();

        let current = fw_dir.join("current");
        let previous = fw_dir.join("previous");
        let temp = fw_dir.join("downloading");

        std::fs::write(&temp, b"new-binary").unwrap();

        // No current exists — should be fine
        assert!(!current.exists());
        std::fs::rename(&temp, &current).unwrap();

        assert_eq!(std::fs::read(&current).unwrap(), b"new-binary");
        assert!(!previous.exists());
    }
}
