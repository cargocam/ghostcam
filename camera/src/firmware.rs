use std::path::Path;
use std::time::Duration;

use anyhow::{Context, Result};
use ghostcam::firmware::{is_newer_version, FirmwareLatestResponse};

/// Compile-time cloud URL for firmware fallback. Set via `GHOSTCAM_CLOUD_URL`
/// env var at build time. Official release builds have this; self-hosted builds
/// from source do not (and that's correct — they shouldn't phone home).
const CLOUD_URL: Option<&str> = option_env!("GHOSTCAM_CLOUD_URL");

/// Timeout for the firmware metadata HTTP request.
const FIRMWARE_CHECK_TIMEOUT: Duration = Duration::from_secs(5);

/// Check for firmware updates before connecting to the server.
///
/// Tries the enrolled server first, then falls back to the cloud URL.
/// This is best-effort: any error results in the camera starting normally.
pub async fn check_for_update(server_addr: &str, data_dir: &Path) {
    let current_version = env!("CARGO_PKG_VERSION");

    // Derive the HTTP URL from the QUIC address. The server_addr is host:quic_port
    // (e.g., "10.0.0.1:4433"). The HTTP port defaults to 3000.
    let http_host = server_addr
        .rsplit_once(':')
        .map(|(host, _)| host)
        .unwrap_or(server_addr);
    let enrolled_url = if http_host.parse::<std::net::IpAddr>().is_ok() {
        format!(
            "http://{http_host}:{}/api/v1/firmware/latest",
            ghostcam::config::HTTP_PORT
        )
    } else {
        format!("https://{http_host}/api/v1/firmware/latest")
    };
    match fetch_firmware_metadata(&enrolled_url).await {
        Ok(resp) => {
            if try_update_from_response(&resp, current_version, data_dir).await {
                return; // Updated (or no update needed) — either way, done
            }
            // Response was valid — don't try cloud fallback even if version was null
            return;
        }
        Err(e) => {
            tracing::warn!("firmware check from enrolled server failed: {e}");
        }
    }

    // Fallback to cloud URL (only if enrolled server was unreachable)
    if let Some(cloud_base) = CLOUD_URL {
        let cloud_url = format!("{cloud_base}/api/v1/firmware/latest");
        match fetch_firmware_metadata(&cloud_url).await {
            Ok(resp) => {
                let _ = try_update_from_response(&resp, current_version, data_dir).await;
            }
            Err(e) => {
                tracing::warn!("firmware check from cloud fallback failed: {e}");
            }
        }
    }

    // If we get here, no update was performed — start normally
    tracing::info!(version = current_version, "firmware is current");
}

/// Fetch firmware metadata from a URL using curl.
async fn fetch_firmware_metadata(url: &str) -> Result<FirmwareLatestResponse> {
    let timeout_secs = FIRMWARE_CHECK_TIMEOUT.as_secs().to_string();
    let output = tokio::process::Command::new("curl")
        .args(["-sf", "--max-time", &timeout_secs, url])
        .output()
        .await
        .context("running curl for firmware check")?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("firmware metadata request failed: {stderr}");
    }

    let resp: FirmwareLatestResponse =
        serde_json::from_slice(&output.stdout).context("parsing firmware metadata response")?;

    Ok(resp)
}

/// Attempt to update based on firmware metadata response.
/// Returns true if the response was successfully processed (whether or not
/// an update was needed), false if something went wrong.
async fn try_update_from_response(
    resp: &FirmwareLatestResponse,
    current_version: &str,
    data_dir: &Path,
) -> bool {
    let Some(ref available_version) = resp.version else {
        tracing::info!("server reports no firmware release available");
        return true;
    };

    if !is_newer_version(current_version, available_version) {
        tracing::info!(
            current = current_version,
            available = available_version,
            "firmware is current or newer"
        );
        return true;
    }

    // Determine our architecture
    let arch = std::env::consts::ARCH;
    // Map Rust arch names to our asset names
    let asset_key = match arch {
        "aarch64" => "aarch64",
        "x86_64" => "x86_64",
        other => {
            tracing::warn!(arch = other, "unsupported architecture for firmware update");
            return true;
        }
    };

    let Some(ref assets) = resp.assets else {
        tracing::warn!("firmware response has version but no assets");
        return true;
    };

    let Some(asset) = assets.get(asset_key) else {
        tracing::warn!(arch = asset_key, "no firmware asset for this architecture");
        return true;
    };

    tracing::info!(
        current = current_version,
        available = available_version,
        arch = asset_key,
        "newer firmware available — downloading"
    );

    // Reuse the existing download/verify/swap logic
    let firmware_dir = data_dir.join("firmware");
    if let Err(e) = tokio::fs::create_dir_all(&firmware_dir).await {
        tracing::error!("failed to create firmware directory: {e}");
        return false;
    }

    let temp_path = firmware_dir.join("downloading");
    let current_path = firmware_dir.join("current");
    let previous_path = firmware_dir.join("previous");

    if let Err(e) = download_and_verify(&asset.url, &asset.sha256, &temp_path, data_dir).await {
        tracing::error!("firmware download/verify failed: {e}");
        return false;
    }

    // Set executable (unix)
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if let Err(e) =
            tokio::fs::set_permissions(&temp_path, std::fs::Permissions::from_mode(0o755)).await
        {
            tracing::error!("failed to set firmware executable permission: {e}");
            return false;
        }
    }

    // Atomic swap: current -> previous, temp -> current
    if current_path.exists() {
        if let Err(e) = tokio::fs::rename(&current_path, &previous_path).await {
            tracing::warn!("failed to preserve previous firmware: {e}");
        }
    }
    if let Err(e) = tokio::fs::rename(&temp_path, &current_path).await {
        tracing::error!("failed to install new firmware: {e}");
        return false;
    }

    // Remove health sentinel so watchdog can detect unhealthy start
    let sentinel = firmware_dir.join("healthy");
    let _ = tokio::fs::remove_file(&sentinel).await;

    tracing::info!(
        version = available_version,
        "firmware installed — exiting for restart"
    );

    // Exit cleanly — watchdog/systemd will restart with new binary
    std::process::exit(0);
}

/// Download a file and verify its SHA-256 hash.
async fn download_and_verify(
    url: &str,
    expected_sha256: &str,
    dest: &Path,
    data_dir: &Path,
) -> Result<()> {
    // Use a simple HTTP GET via tokio (avoid adding reqwest dep)
    // For now, support file:// URLs for testing, and shell out to curl for http(s)
    let bytes = if url.starts_with("file://") {
        let raw = url.strip_prefix("file://").unwrap();
        let resolved = std::fs::canonicalize(raw).context("resolving firmware file path")?;
        let canon_data_dir =
            std::fs::canonicalize(data_dir).context("resolving data dir for path check")?;
        anyhow::ensure!(
            resolved.starts_with(&canon_data_dir),
            "firmware file path escapes data dir"
        );
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

        download_and_verify(&url, &hash, &dest, dir.path())
            .await
            .unwrap();
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

        let result = download_and_verify(&url, "badhash", &dest, dir.path()).await;
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
