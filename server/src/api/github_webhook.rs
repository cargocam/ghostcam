use std::collections::HashMap;
use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use ghostcam::firmware::{FirmwareAsset, FirmwareRelease};
use ring::hmac;

use super::state::AppState;

/// POST /api/v1/webhooks/github
///
/// Public endpoint (no auth middleware). Verified by GitHub webhook HMAC signature.
pub async fn github_webhook(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let secret = match &state.github_webhook_secret {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    // Verify signature
    let signature = match headers
        .get("x-hub-signature-256")
        .and_then(|v| v.to_str().ok())
    {
        Some(s) => s,
        None => return StatusCode::BAD_REQUEST.into_response(),
    };

    if !verify_github_signature(secret.as_bytes(), &body, signature) {
        tracing::warn!("GitHub webhook signature verification failed");
        return StatusCode::BAD_REQUEST.into_response();
    }

    // Parse event type from header
    let event_type = match headers.get("x-github-event").and_then(|v| v.to_str().ok()) {
        Some(t) => t.to_string(),
        None => return StatusCode::BAD_REQUEST.into_response(),
    };

    if event_type != "release" {
        // Ignore non-release events (ping, push, etc.)
        return StatusCode::OK.into_response();
    }

    let payload: serde_json::Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            tracing::warn!("GitHub webhook JSON parse error: {e}");
            return StatusCode::BAD_REQUEST.into_response();
        }
    };

    let action = payload["action"].as_str().unwrap_or("");
    if action != "published" {
        return StatusCode::OK.into_response();
    }

    let release = &payload["release"];
    let tag = release["tag_name"].as_str().unwrap_or("");
    let version = tag.strip_prefix('v').unwrap_or(tag).to_string();

    if version.is_empty() {
        tracing::warn!("GitHub release webhook with empty tag");
        return StatusCode::OK.into_response();
    }

    tracing::info!(version = %version, "GitHub release webhook received");

    // Parse assets — look for camera binaries and checksums.txt
    let assets = release["assets"].as_array();
    let Some(assets) = assets else {
        tracing::warn!("GitHub release has no assets");
        return StatusCode::OK.into_response();
    };

    // Find checksums.txt asset and fetch it
    let checksums_url = assets.iter().find_map(|a| {
        let name = a["name"].as_str().unwrap_or("");
        if name == "checksums.txt" {
            a["browser_download_url"].as_str().map(|s| s.to_string())
        } else {
            None
        }
    });

    let checksums = if let Some(url) = checksums_url {
        match fetch_checksums(&url).await {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!("failed to fetch checksums.txt: {e}");
                HashMap::new()
            }
        }
    } else {
        HashMap::new()
    };

    // Build asset map
    let mut firmware_assets = HashMap::new();
    for asset in assets {
        let name = asset["name"].as_str().unwrap_or("");
        let url = asset["browser_download_url"]
            .as_str()
            .unwrap_or("")
            .to_string();

        // Match known binary patterns: ghostcam-camera-{arch}
        let arch = if name.contains("aarch64") {
            "aarch64"
        } else if name.contains("x86_64") {
            "x86_64"
        } else {
            continue;
        };

        // Skip checksums.txt itself
        if name == "checksums.txt" {
            continue;
        }

        let sha256 = checksums.get(name).cloned().unwrap_or_default();
        firmware_assets.insert(
            arch.to_string(),
            FirmwareAsset {
                url,
                sha256,
            },
        );
    }

    if firmware_assets.is_empty() {
        tracing::warn!(version = %version, "no camera firmware assets found in release");
        return StatusCode::OK.into_response();
    }

    let fw_release = FirmwareRelease {
        version: version.clone(),
        assets: firmware_assets,
    };

    // Update in-memory state
    *state.firmware_release.write().await = Some(fw_release.clone());
    tracing::info!(version = %version, "firmware release updated");

    // Publish to Redis pub/sub for other server instances
    if let Some(ref redis) = state.redis {
        if let Ok(json) = serde_json::to_string(&fw_release) {
            crate::redis::firmware::publish_release(redis, &json).await;
        }
    }

    // Schedule staggered reboot of connected cameras
    crate::firmware::schedule_staggered_reboot(
        state.registry.clone(),
        state.update_stagger_secs,
    );

    StatusCode::OK.into_response()
}

/// Verify the `X-Hub-Signature-256` header using HMAC-SHA256.
pub fn verify_github_signature(secret: &[u8], body: &[u8], signature_header: &str) -> bool {
    let Some(hex_sig) = signature_header.strip_prefix("sha256=") else {
        return false;
    };

    let Ok(expected) = hex::decode(hex_sig) else {
        return false;
    };

    let key = hmac::Key::new(hmac::HMAC_SHA256, secret);
    hmac::verify(&key, body, &expected).is_ok()
}

/// Fetch checksums.txt and parse into filename -> sha256 map.
async fn fetch_checksums(url: &str) -> anyhow::Result<HashMap<String, String>> {
    let output = tokio::process::Command::new("curl")
        .args(["-sfL", "--max-time", "30", url])
        .output()
        .await?;

    if !output.status.success() {
        anyhow::bail!("checksums.txt download failed");
    }

    let text = String::from_utf8_lossy(&output.stdout);
    let mut checksums = HashMap::new();

    for line in text.lines() {
        // Format: "sha256_hash  filename" or "sha256_hash filename"
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() >= 2 {
            let hash = parts[0].to_string();
            let filename = parts[parts.len() - 1].to_string();
            checksums.insert(filename, hash);
        }
    }

    Ok(checksums)
}

/// Fetch the latest release from GitHub API on server startup.
pub async fn fetch_latest_github_release(repo: &str) -> Option<FirmwareRelease> {
    let api_url = format!("https://api.github.com/repos/{repo}/releases/latest");

    let output = tokio::process::Command::new("curl")
        .args([
            "-sf",
            "--max-time",
            "10",
            "-H",
            "Accept: application/vnd.github+json",
            "-H",
            "User-Agent: ghostcam-server",
            &api_url,
        ])
        .output()
        .await
        .ok()?;

    if !output.status.success() {
        tracing::warn!("GitHub API request failed for latest release");
        return None;
    }

    let payload: serde_json::Value = serde_json::from_slice(&output.stdout).ok()?;

    let tag = payload["tag_name"].as_str()?;
    let version = tag.strip_prefix('v').unwrap_or(tag).to_string();

    let assets = payload["assets"].as_array()?;

    // Find checksums.txt
    let checksums_url = assets.iter().find_map(|a| {
        let name = a["name"].as_str().unwrap_or("");
        if name == "checksums.txt" {
            a["browser_download_url"].as_str().map(|s| s.to_string())
        } else {
            None
        }
    });

    let checksums = if let Some(url) = checksums_url {
        fetch_checksums(&url).await.unwrap_or_default()
    } else {
        HashMap::new()
    };

    let mut firmware_assets = HashMap::new();
    for asset in assets {
        let name = asset["name"].as_str().unwrap_or("");
        let url = asset["browser_download_url"]
            .as_str()
            .unwrap_or("")
            .to_string();

        let arch = if name.contains("aarch64") {
            "aarch64"
        } else if name.contains("x86_64") {
            "x86_64"
        } else {
            continue;
        };

        if name == "checksums.txt" {
            continue;
        }

        let sha256 = checksums.get(name).cloned().unwrap_or_default();
        firmware_assets.insert(arch.to_string(), FirmwareAsset { url, sha256 });
    }

    if firmware_assets.is_empty() {
        return None;
    }

    Some(FirmwareRelease {
        version,
        assets: firmware_assets,
    })
}

/// Simple hex decoding (avoids adding hex crate dependency).
mod hex {
    pub fn decode(s: &str) -> Result<Vec<u8>, ()> {
        if !s.len().is_multiple_of(2) {
            return Err(());
        }
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).map_err(|_| ()))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verify_signature_valid() {
        let secret = b"test-secret";
        let body = b"hello world";
        let key = hmac::Key::new(hmac::HMAC_SHA256, secret);
        let tag = hmac::sign(&key, body);
        let hex_sig = tag
            .as_ref()
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>();
        let header = format!("sha256={hex_sig}");

        assert!(verify_github_signature(secret, body, &header));
    }

    #[test]
    fn verify_signature_invalid() {
        let secret = b"test-secret";
        let body = b"hello world";
        let header = "sha256=0000000000000000000000000000000000000000000000000000000000000000";

        assert!(!verify_github_signature(secret, body, header));
    }

    #[test]
    fn verify_signature_bad_format() {
        assert!(!verify_github_signature(b"secret", b"body", "not-sha256"));
        assert!(!verify_github_signature(
            b"secret",
            b"body",
            "sha256=not-hex!"
        ));
    }

    #[test]
    fn hex_decode_works() {
        assert_eq!(hex::decode("48656c6c6f").unwrap(), b"Hello");
        assert_eq!(hex::decode("").unwrap(), Vec::<u8>::new());
        assert!(hex::decode("0").is_err());
        assert!(hex::decode("zz").is_err());
    }
}
