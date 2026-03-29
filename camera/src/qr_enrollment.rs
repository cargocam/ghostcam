//! QR code enrollment: scan for a QR code containing server URL + enrollment JWT,
//! then complete the standard enrollment flow.
//!
//! On Linux with rpicam-still available, captures raw YUV420 frames and decodes QR codes
//! from the Y (grayscale) plane. On other platforms (macOS), this module compiles but
//! returns an error indicating QR scanning is unavailable.

use anyhow::Result;

#[cfg(target_os = "linux")]
use std::path::Path;
#[cfg(target_os = "linux")]
use std::time::Duration;

#[cfg(target_os = "linux")]
use anyhow::Context;
#[cfg(target_os = "linux")]
use crate::enrollment;
#[cfg(any(target_os = "linux", test))]
use serde::Deserialize;

/// QR code JSON payload.
#[cfg(any(target_os = "linux", test))]
#[derive(Debug, Deserialize)]
struct QrPayload {
    /// Server HTTP base URL (e.g. "http://10.0.0.1:3000")
    s: String,
    /// Enrollment JWT
    t: String,
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

/// Scan for a QR code and complete enrollment.
///
/// On Linux, uses `rpicam-still` to capture raw YUV420 frames. On other platforms,
/// returns an error (QR scanning requires rpicam-still).
pub async fn scan_and_enroll(
    data_dir: &str,
    device_cert: &[u8],
    device_key: &[u8],
) -> Result<()> {
    #[cfg(target_os = "linux")]
    {
        scan_and_enroll_linux(data_dir, device_cert, device_key).await
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = (data_dir, device_cert, device_key);
        anyhow::bail!("QR code scanning requires rpicam-still (Linux only)")
    }
}

/// Linux implementation: capture frames via rpicam-still and scan for QR codes.
#[cfg(target_os = "linux")]
async fn scan_and_enroll_linux(
    data_dir: &str,
    device_cert: &[u8],
    device_key: &[u8],
) -> Result<()> {
    use tokio::io::AsyncReadExt;
    use tokio::process::Command;

    tracing::info!("starting QR code scan (timeout: {}s)", QR_SCAN_TIMEOUT.as_secs());

    // Try rpicam-still first, fall back to libcamera-still
    let camera_bin = if which_exists("rpicam-still").await {
        "rpicam-still"
    } else if which_exists("libcamera-still").await {
        "libcamera-still"
    } else {
        anyhow::bail!(
            "neither rpicam-still nor libcamera-still found; \
             install libcamera-apps or provide --enrollment-jwt"
        );
    };

    tracing::info!(binary = camera_bin, "using camera binary for QR scanning");

    let mut child = Command::new(camera_bin)
        .args([
            "--width",
            &FRAME_WIDTH.to_string(),
            "--height",
            &FRAME_HEIGHT.to_string(),
            "-n",           // no preview
            "-t",
            "0",            // run indefinitely (we kill it)
            "--timelapse",
            &CAPTURE_INTERVAL.as_millis().to_string(),
            "--encoding",
            "yuv420",
            "-o",
            "-",            // stdout
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
            stdout.read_exact(&mut buf).await.context("reading frame from camera")?;
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
    let qr: QrPayload = serde_json::from_str(&payload_str)
        .context("invalid QR code payload (expected JSON with 's' and 't' fields)")?;

    if qr.s.is_empty() || qr.t.is_empty() {
        anyhow::bail!("QR code payload has empty server URL or token");
    }

    tracing::info!(server = %qr.s, "QR enrollment payload decoded");

    // Parse the JWT to extract the server QUIC address
    let enrollment_data = enrollment::parse_enrollment_jwt(&qr.t)?;

    // Run the standard enrollment flow
    let result = enrollment::enroll(&enrollment_data, device_cert, device_key).await?;

    // Store enrollment data
    enrollment::store_enrollment(
        Path::new(data_dir),
        &result,
        &enrollment_data.server_addr,
    )
    .await?;

    tracing::info!("QR enrollment complete");
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
    fn parse_qr_payload() {
        let json = r#"{"s":"http://10.0.0.1:3000","t":"eyJhbGciOiJFUzI1NiJ9.test.sig"}"#;
        let qr: QrPayload = serde_json::from_str(json).unwrap();
        assert_eq!(qr.s, "http://10.0.0.1:3000");
        assert_eq!(qr.t, "eyJhbGciOiJFUzI1NiJ9.test.sig");
    }

    #[test]
    fn parse_qr_payload_missing_field() {
        let json = r#"{"s":"http://10.0.0.1:3000"}"#;
        let result: Result<QrPayload, _> = serde_json::from_str(json);
        assert!(result.is_err());
    }

    #[test]
    fn try_decode_qr_short_buffer() {
        // Buffer too small for a 640x480 Y plane
        let buf = vec![0u8; 100];
        assert!(try_decode_qr(&buf, 640, 480).is_none());
    }

    #[test]
    fn try_decode_qr_no_qr_in_noise() {
        // Random-ish data, no QR code present
        let y_size = (FRAME_WIDTH * FRAME_HEIGHT) as usize;
        let frame_size = y_size * 3 / 2;
        let buf = vec![128u8; frame_size]; // uniform gray
        assert!(try_decode_qr(&buf, FRAME_WIDTH, FRAME_HEIGHT).is_none());
    }
}
