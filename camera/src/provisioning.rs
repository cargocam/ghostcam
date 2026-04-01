use std::path::Path;
use std::time::Duration;

use anyhow::Result;
use tokio_util::sync::CancellationToken;

use crate::http_client;
use crate::qr_enrollment::QrResult;

/// Run the provisioning flow: scan QR → configure WiFi → provision with server.
///
/// Blocks until provisioning succeeds or is cancelled.
/// Returns (api_key, device_id, server_url).
pub async fn run_provisioning(
    data_dir: &Path,
    device_serial: &str,
    cancel: CancellationToken,
) -> Result<(String, String, String)> {
    tracing::info!("entering provisioning mode — waiting for QR code");

    // Check for pre-provisioned token file (Docker auto-provision)
    let token_path = data_dir.join("provision_token");
    let server_url_path = data_dir.join("server_url");
    if token_path.exists() && server_url_path.exists() {
        let token = tokio::fs::read_to_string(&token_path).await?;
        let server_url = tokio::fs::read_to_string(&server_url_path).await?;
        let token = token.trim();
        let server_url = server_url.trim();

        if !token.is_empty() && !server_url.is_empty() {
            tracing::info!("found pre-provisioned token file, attempting provisioning");
            match http_client::provision(server_url, token, device_serial).await {
                Ok(resp) => {
                    http_client::save_credentials(
                        data_dir,
                        &resp.api_key,
                        &resp.device_id,
                        server_url,
                    )?;
                    let _ = tokio::fs::remove_file(&token_path).await;
                    return Ok((resp.api_key, resp.device_id, server_url.to_string()));
                }
                Err(e) => {
                    tracing::warn!("pre-provisioned token failed: {e}, falling through to QR");
                }
            }
        }
    }

    // QR scanning loop
    loop {
        if cancel.is_cancelled() {
            anyhow::bail!("provisioning cancelled");
        }

        match scan_and_provision(data_dir, device_serial).await {
            Ok(creds) => return Ok(creds),
            Err(e) => {
                tracing::warn!("provisioning attempt failed: {e}");
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(5)) => {}
                    _ = cancel.cancelled() => anyhow::bail!("provisioning cancelled"),
                }
            }
        }
    }
}

/// Single attempt: scan a QR code, extract server URL + token, provision.
async fn scan_and_provision(
    data_dir: &Path,
    device_serial: &str,
) -> Result<(String, String, String)> {
    let qr_result = crate::qr_enrollment::scan_for_claim_token().await?;

    let (token, server_url) = match qr_result {
        QrResult::Wifi {
            ssid,
            claim_token,
            server_addr,
        } => {
            // WiFi QR — configure WiFi, extract token
            tracing::info!(ssid = %ssid, "configuring WiFi from QR code");
            if let Err(e) = crate::network::ensure_wifi(&ssid, None).await {
                tracing::warn!("WiFi configuration failed: {e}");
            }
            tokio::time::sleep(Duration::from_secs(3)).await;

            let token = claim_token.ok_or_else(|| {
                anyhow::anyhow!("QR code has WiFi but no provisioning token")
            })?;
            let server_url = server_addr.ok_or_else(|| {
                anyhow::anyhow!("QR code has no server URL")
            })?;
            (token, server_url)
        }
        QrResult::ClaimToken { token, server_addr } => {
            let server_url = server_addr.ok_or_else(|| {
                anyhow::anyhow!("QR code has no server URL")
            })?;
            (token, server_url)
        }
    };

    // The server_addr from the old QR format is host:port (QUIC).
    // For the new flow, the `s` field should be a full HTTPS URL.
    // Support both: if it looks like a URL, use as-is; otherwise, wrap it.
    let server_url = if server_url.starts_with("http://") || server_url.starts_with("https://") {
        server_url
    } else {
        format!("https://{server_url}")
    };

    tracing::info!(server = %server_url, "provisioning with server");
    let resp = http_client::provision(&server_url, &token, device_serial).await?;

    http_client::save_credentials(data_dir, &resp.api_key, &resp.device_id, &server_url)?;

    tracing::info!(device_id = %resp.device_id, "provisioning complete");

    Ok((resp.api_key, resp.device_id, server_url))
}
