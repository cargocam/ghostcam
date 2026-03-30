//! QR code scanning for enrollment and WiFi setup.
//!
//! Two QR code types distinguished by payload fields:
//! - WiFi QR: has `w` field → `{"w": "ssid", "p": "password"}`
//! - Claim QR: has `t` field → `{"t": "claim_jwt"}`
//! - Combined: `{"w": "ssid", "p": "password", "t": "claim_jwt"}` — configure WiFi AND claim
//!
//! On Linux with rpicam-still available, captures raw YUV420 frames and decodes QR codes
//! from the Y (grayscale) plane. On other platforms, returns an error.

use anyhow::Result;

#[cfg(target_os = "linux")]
use std::time::Duration;

#[cfg(any(target_os = "linux", test))]
use serde::Deserialize;

/// QR code JSON payload — all fields optional to support different QR types.
#[cfg(any(target_os = "linux", test))]
#[derive(Debug, Deserialize)]
pub struct QrPayload {
    /// WiFi SSID (presence indicates WiFi QR)
    #[serde(default)]
    pub w: Option<String>,
    /// WiFi password
    #[serde(default)]
    pub p: Option<String>,
    /// Claim JWT (presence indicates claim QR)
    #[serde(default)]
    pub t: Option<String>,
    /// Server URL (legacy field, accepted for backward compat but unused)
    #[serde(default)]
    #[allow(dead_code)]
    pub s: Option<String>,
}

/// Result of scanning a QR code.
#[derive(Debug)]
#[allow(dead_code)]
pub enum QrResult {
    /// WiFi credentials were found and configured. Optionally also has a claim token.
    Wifi {
        ssid: String,
        claim_token: Option<String>,
    },
    /// Claim token only (camera already has network).
    ClaimToken(String),
}

/// Maximum time to scan for a QR code before giving up.
#[cfg(target_os = "linux")]
const QR_SCAN_TIMEOUT: Duration = Duration::from_secs(5 * 60);

/// Interval between frame captures.
#[cfg(target_os = "linux")]
const CAPTURE_INTERVAL: Duration = Duration::from_millis(500);

/// Frame dimensions for QR scanning.
#[cfg(any(target_os = "linux", test))]
const FRAME_WIDTH: u32 = 640;
#[cfg(any(target_os = "linux", test))]
const FRAME_HEIGHT: u32 = 480;

/// Attempt to decode a QR code from a YUV420 frame's Y (grayscale) plane.
#[cfg(any(target_os = "linux", test))]
fn try_decode_qr(yuv_data: &[u8], width: u32, height: u32) -> Option<String> {
    let y_size = (width * height) as usize;
    if yuv_data.len() < y_size {
        return None;
    }
    let gray = image::GrayImage::from_raw(width, height, yuv_data[..y_size].to_vec())?;
    let mut prepared = rqrr::PreparedImage::prepare(gray);
    let grids = prepared.detect_grids();
    for grid in grids {
        if let Ok((_, content)) = grid.decode() {
            return Some(content);
        }
    }
    None
}

/// Scan for a claim QR code and return the token.
///
/// On Linux, uses `rpicam-still` to capture raw YUV420 frames. On other platforms,
/// returns an error (QR scanning requires rpicam-still).
#[allow(dead_code)]
pub async fn scan_for_claim_token() -> Result<QrResult> {
    #[cfg(target_os = "linux")]
    {
        scan_for_qr_linux().await
    }

    #[cfg(not(target_os = "linux"))]
    {
        anyhow::bail!("QR code scanning requires rpicam-still (Linux only)")
    }
}

/// Linux implementation: capture frames via rpicam-still and scan for QR codes.
#[cfg(target_os = "linux")]
async fn scan_for_qr_linux() -> Result<QrResult> {
    use anyhow::Context;
    use tokio::io::AsyncReadExt;
    use tokio::process::Command;

    tracing::info!(
        "starting QR code scan (timeout: {}s)",
        QR_SCAN_TIMEOUT.as_secs()
    );

    // Try rpicam-still first, fall back to libcamera-still
    let camera_bin = if which_exists("rpicam-still").await {
        "rpicam-still"
    } else if which_exists("libcamera-still").await {
        "libcamera-still"
    } else {
        anyhow::bail!(
            "neither rpicam-still nor libcamera-still found; \
             cannot scan QR codes"
        );
    };

    tracing::info!(binary = camera_bin, "using camera binary for QR scanning");

    let mut child = Command::new(camera_bin)
        .args([
            "--width",
            &FRAME_WIDTH.to_string(),
            "--height",
            &FRAME_HEIGHT.to_string(),
            "-n", // no preview
            "-t",
            "0", // run indefinitely (we kill it)
            "--timelapse",
            &CAPTURE_INTERVAL.as_millis().to_string(),
            "--encoding",
            "yuv420",
            "-o",
            "-", // stdout
        ])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null())
        .spawn()
        .context("failed to spawn camera capture process")?;

    let mut stdout = child
        .stdout
        .take()
        .context("failed to get stdout from camera process")?;

    // YUV420 frame size: width * height * 3 / 2
    let frame_size = (FRAME_WIDTH * FRAME_HEIGHT * 3 / 2) as usize;
    let mut buf = vec![0u8; frame_size];
    let mut frames_scanned = 0u32;

    let result = tokio::time::timeout(QR_SCAN_TIMEOUT, async {
        loop {
            // Read one complete YUV420 frame
            stdout
                .read_exact(&mut buf)
                .await
                .context("reading frame from camera")?;
            frames_scanned += 1;

            if frames_scanned % 10 == 1 {
                tracing::debug!(frames = frames_scanned, "scanning for QR code...");
            }

            if let Some(payload_str) = try_decode_qr(&buf, FRAME_WIDTH, FRAME_HEIGHT) {
                tracing::info!(frames = frames_scanned, "QR code detected");
                return Ok(payload_str);
            }
        }
    })
    .await;

    // Kill the camera process regardless of outcome
    let _ = child.kill().await;

    let payload_str = match result {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => return Err(e),
        Err(_) => {
            anyhow::bail!(
                "QR code scan timed out after {}s ({} frames scanned)",
                QR_SCAN_TIMEOUT.as_secs(),
                frames_scanned
            );
        }
    };

    // Parse the QR payload
    let qr: QrPayload =
        serde_json::from_str(&payload_str).context("invalid QR code payload (expected JSON)")?;

    // Handle WiFi QR
    if let Some(ssid) = &qr.w {
        let psk = qr.p.as_deref().unwrap_or("");
        tracing::info!(ssid = %ssid, "WiFi QR detected — configuring network");
        configure_wifi(ssid, psk).await?;
        return Ok(QrResult::Wifi {
            ssid: ssid.clone(),
            claim_token: qr.t,
        });
    }

    // Handle claim-only QR
    if let Some(token) = qr.t {
        if token.is_empty() {
            anyhow::bail!("QR code has empty claim token");
        }
        return Ok(QrResult::ClaimToken(token));
    }

    anyhow::bail!("QR code payload has neither WiFi ('w') nor claim token ('t') field");
}

/// Configure WiFi via nmcli.
#[cfg(target_os = "linux")]
async fn configure_wifi(ssid: &str, psk: &str) -> Result<()> {
    use anyhow::Context;

    let status = tokio::process::Command::new("nmcli")
        .args(["device", "wifi", "connect", ssid, "password", psk])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::piped())
        .status()
        .await
        .context("failed to run nmcli")?;

    if !status.success() {
        anyhow::bail!("nmcli failed to connect to WiFi network '{ssid}'");
    }

    tracing::info!(ssid = %ssid, "WiFi configured successfully");
    Ok(())
}

/// Check if a binary exists on PATH.
#[cfg(target_os = "linux")]
async fn which_exists(name: &str) -> bool {
    tokio::process::Command::new("which")
        .arg(name)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .await
        .map(|s| s.success())
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_claim_only_qr() {
        let json = r#"{"t":"eyJhbGciOiJFUzI1NiJ9.test.sig"}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert!(qr.w.is_none());
        assert_eq!(qr.t.as_deref(), Some("eyJhbGciOiJFUzI1NiJ9.test.sig"));
    }

    #[test]
    fn parse_wifi_only_qr() {
        let json = r#"{"w":"MyNetwork","p":"secret123"}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert_eq!(qr.w.as_deref(), Some("MyNetwork"));
        assert_eq!(qr.p.as_deref(), Some("secret123"));
        assert!(qr.t.is_none());
    }

    #[test]
    fn parse_combined_qr() {
        let json = r#"{"w":"MyNetwork","p":"secret123","t":"eyJ.claim.jwt"}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert_eq!(qr.w.as_deref(), Some("MyNetwork"));
        assert_eq!(qr.t.as_deref(), Some("eyJ.claim.jwt"));
    }

    #[test]
    fn parse_legacy_qr_with_server_url() {
        // Legacy QR codes had an 's' field — should still parse
        let json = r#"{"s":"http://10.0.0.1:3000","t":"eyJ.test.sig"}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert_eq!(qr.s.as_deref(), Some("http://10.0.0.1:3000"));
        assert_eq!(qr.t.as_deref(), Some("eyJ.test.sig"));
    }

    #[test]
    fn parse_empty_payload_fails() {
        // No w or t field — will parse but is useless
        let json = r#"{}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert!(qr.w.is_none());
        assert!(qr.t.is_none());
    }

    #[test]
    fn try_decode_qr_short_buffer() {
        let buf = vec![0u8; 100];
        assert!(try_decode_qr(&buf, 640, 480).is_none());
    }

    #[test]
    fn try_decode_qr_no_qr_in_noise() {
        let y_size = (FRAME_WIDTH * FRAME_HEIGHT) as usize;
        let frame_size = y_size * 3 / 2;
        let buf = vec![128u8; frame_size];
        assert!(try_decode_qr(&buf, FRAME_WIDTH, FRAME_HEIGHT).is_none());
    }
}
