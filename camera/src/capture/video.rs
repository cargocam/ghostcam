//! Real video capture via rpicam-vid / libcamera-vid.
//!
//! Spawns the system capture binary as a subprocess, reads raw H.264 Annex B
//! from stdout in a `spawn_blocking` task, parses into individual NAL units,
//! and sends each as a `CaptureMessage::VideoNal`.

use std::io::Read;
use std::process::Stdio;

use anyhow::{Context, Result};
use bytes::Bytes;
use tokio::process::Command;
use tokio_util::sync::CancellationToken;

use super::{CaptureMessage, CaptureSender};
use crate::config::CameraConfig;

/// Detect which capture binary is available on the system.
/// Returns `Some("rpicam-vid")` or `Some("libcamera-vid")`, or `None`.
pub async fn detect_capture_binary() -> Option<&'static str> {
    for bin in ["rpicam-vid", "libcamera-vid"] {
        if Command::new("which")
            .arg(bin)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .await
            .map(|s| s.success())
            .unwrap_or(false)
        {
            return Some(bin);
        }
    }
    None
}

/// Start real video capture from rpicam-vid / libcamera-vid.
///
/// Spawns the capture subprocess, reads H.264 Annex B from stdout, parses
/// into individual NAL units, and sends each via the provided channel.
/// On EOF or cancellation, the subprocess is killed and the function returns.
pub async fn run_real_video(
    config: &CameraConfig,
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()> {
    let binary = detect_capture_binary()
        .await
        .context("neither rpicam-vid nor libcamera-vid found on PATH")?;

    let mut args = vec![
        "-t".to_string(),
        "0".to_string(), // run indefinitely
        "--width".to_string(),
        config.video_width.to_string(),
        "--height".to_string(),
        config.video_height.to_string(),
        "--framerate".to_string(),
        config.video_fps.to_string(),
        "--codec".to_string(),
        "h264".to_string(),
        "--profile".to_string(),
        "baseline".to_string(),
        "-o".to_string(),
        "-".to_string(),
        "--flush".to_string(),
        "-n".to_string(),
        "--inline".to_string(),
    ];

    if config.video_bitrate > 0 {
        args.push("--bitrate".to_string());
        args.push(config.video_bitrate.to_string());
    }

    if config.video_keyframe_interval > 0 {
        args.push("--intra".to_string());
        args.push(config.video_keyframe_interval.to_string());
    }

    tracing::info!(
        binary,
        width = config.video_width,
        height = config.video_height,
        fps = config.video_fps,
        bitrate = config.video_bitrate,
        keyframe_interval = config.video_keyframe_interval,
        "starting video capture"
    );
    tracing::debug!(binary, ?args, "capture args");

    let mut child = Command::new(binary)
        .args(&args)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .with_context(|| format!("failed to spawn {binary} — is it installed?"))?;

    let stdout = child
        .stdout
        .take()
        .context("failed to capture stdout from capture process")?;

    // Convert tokio stdout to std for use in spawn_blocking
    let stdout = stdout
        .into_owned_fd()
        .context("failed to convert stdout to owned fd")?;
    let stdout = std::fs::File::from(stdout);

    // Spawn blocking reader that parses Annex B into NAL units
    let reader_cancel = cancel.clone();
    let reader_handle = tokio::task::spawn_blocking(move || {
        read_and_parse_nals(stdout, tx, reader_cancel);
    });

    // Spawn a task to log stderr output
    let stderr = child.stderr.take();
    let stderr_task = tokio::spawn(async move {
        if let Some(mut stderr) = stderr {
            use tokio::io::AsyncReadExt;
            let mut buf = String::new();
            let _ = stderr.read_to_string(&mut buf).await;
            if !buf.is_empty() {
                for line in buf.lines() {
                    tracing::debug!(source = "rpicam-vid", "{}", line);
                }
            }
        }
    });

    // Wait for either cancellation or the reader to finish
    tokio::select! {
        _ = cancel.cancelled() => {
            tracing::info!("video capture cancelled, stopping subprocess");
            // kill_on_drop handles cleanup, but let's be explicit
            let _ = child.kill().await;
        }
        _ = reader_handle => {
            tracing::info!("video reader finished (process exited)");
            // Check exit status
            match child.try_wait() {
                Ok(Some(status)) => {
                    if !status.success() {
                        tracing::warn!(?status, "capture process exited with error");
                    }
                }
                Ok(None) => {
                    let _ = child.kill().await;
                }
                Err(e) => {
                    tracing::warn!("failed to check capture process status: {e}");
                }
            }
        }
    }

    let _ = stderr_task.await;

    Ok(())
}

/// Read raw H.264 Annex B from a reader, parse into individual NAL units,
/// and send each as a `CaptureMessage::VideoNal`.
///
/// This is an incremental parser that handles partial reads across buffer
/// boundaries. It searches for Annex B start codes (0x00 0x00 0x01 or
/// 0x00 0x00 0x00 0x01) to delimit NAL units.
fn read_and_parse_nals<R: Read>(mut reader: R, tx: CaptureSender, cancel: CancellationToken) {
    let mut buf = vec![0u8; 16384]; // 16KB read buffer
    let mut accum = Vec::with_capacity(65536); // accumulates data between start codes
    let mut found_first_start_code = false;
    let mut total_nals = 0u64;
    let mut total_bytes = 0u64;

    loop {
        if cancel.is_cancelled() {
            break;
        }

        let n = match reader.read(&mut buf) {
            Ok(0) => {
                tracing::info!("video stream EOF");
                break;
            }
            Ok(n) => n,
            Err(e) => {
                tracing::error!("error reading video stream: {e}");
                break;
            }
        };

        total_bytes += n as u64;
        accum.extend_from_slice(&buf[..n]);

        // On first data, skip past the initial Annex B start code so the
        // accumulator only contains NAL body bytes.
        if !found_first_start_code {
            if let Some(body_start) = skip_leading_start_code(&accum) {
                accum.drain(..body_start);
                found_first_start_code = true;
            } else {
                // Haven't accumulated enough bytes to see a start code yet
                continue;
            }
        }

        // Parse all complete NAL units from the accumulator
        while let Some((nal_end, next_start)) = find_next_nal_boundary(&accum) {
            // We found a start code at position nal_end.
            // Everything before it is a NAL unit body.
            if nal_end > 0 {
                let nal_data = Bytes::copy_from_slice(&accum[..nal_end]);
                if tx
                    .blocking_send(CaptureMessage::VideoNal(nal_data))
                    .is_err()
                {
                    tracing::info!("video receiver dropped, stopping capture");
                    return;
                }
                total_nals += 1;

                if total_nals.is_multiple_of(300) {
                    tracing::debug!(total_nals, total_bytes, "video capture progress");
                }
            }
            // Remove the consumed NAL and the start code prefix
            accum.drain(..next_start);
        }
    }

    // Flush any remaining data as the last NAL
    if !accum.is_empty() {
        let nal_data = Bytes::copy_from_slice(&accum);
        let _ = tx.blocking_send(CaptureMessage::VideoNal(nal_data));
        total_nals += 1;
    }

    tracing::info!(total_nals, total_bytes, "video capture finished");
}

/// Skip the leading Annex B start code at the beginning of a buffer.
/// Returns the byte offset where the NAL body begins, or `None` if the
/// buffer doesn't start with a recognizable start code yet.
fn skip_leading_start_code(data: &[u8]) -> Option<usize> {
    if data.len() >= 4 && data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x00 && data[3] == 0x01 {
        Some(4)
    } else if data.len() >= 3 && data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x01 {
        Some(3)
    } else if data.len() >= 4 {
        // Enough data but no start code — could be mid-stream data without a leading start code.
        // Treat position 0 as start of NAL body.
        Some(0)
    } else {
        None // Need more data
    }
}

/// Find the next Annex B start code boundary in the buffer.
///
/// Searches for 0x00 0x00 0x01 (3-byte) or 0x00 0x00 0x00 0x01 (4-byte) start
/// codes starting from position 1 (skipping the very beginning, which is the
/// start of the current NAL).
///
/// Returns `Some((nal_end, body_start))` where:
/// - `nal_end` is the byte offset where the current NAL data ends
/// - `body_start` is the byte offset where the next NAL body begins (after the start code)
///
/// Returns `None` if no start code is found (need more data).
fn find_next_nal_boundary(data: &[u8]) -> Option<(usize, usize)> {
    // Need at least 4 bytes to find a start code after position 0
    if data.len() < 4 {
        return None;
    }

    let mut i = 1; // skip position 0 — that's inside the current NAL
    while i < data.len().saturating_sub(2) {
        if data[i] == 0x00 && data[i + 1] == 0x00 {
            // Check for 4-byte start code: 00 00 00 01
            if i + 3 < data.len() && data[i + 2] == 0x00 && data[i + 3] == 0x01 {
                return Some((i, i + 4));
            }
            // Check for 3-byte start code: 00 00 01
            if data[i + 2] == 0x01 {
                return Some((i, i + 3));
            }
        }
        i += 1;
    }

    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn find_boundary_3byte() {
        // NAL data + 3-byte start code + next NAL data
        let data = [0x65, 0xAA, 0xBB, 0x00, 0x00, 0x01, 0x67, 0xCC];
        let result = find_next_nal_boundary(&data);
        assert_eq!(result, Some((3, 6)));
    }

    #[test]
    fn find_boundary_4byte() {
        let data = [0x65, 0xAA, 0x00, 0x00, 0x00, 0x01, 0x67, 0xCC];
        let result = find_next_nal_boundary(&data);
        assert_eq!(result, Some((2, 6)));
    }

    #[test]
    fn find_boundary_none_when_no_start_code() {
        let data = [0x65, 0xAA, 0xBB, 0xCC];
        assert!(find_next_nal_boundary(&data).is_none());
    }

    #[test]
    fn find_boundary_none_when_too_short() {
        let data = [0x65, 0xAA];
        assert!(find_next_nal_boundary(&data).is_none());
    }

    #[test]
    fn read_and_parse_produces_nals() {
        // Simulate an Annex B stream: SPS + PPS + IDR
        let stream: Vec<u8> = vec![
            // SPS (with 4-byte start code)
            0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1e,
            // PPS (with 3-byte start code)
            0x00, 0x00, 0x01, 0x68, 0xce, 0x38, 0x80,
            // IDR slice (with 4-byte start code)
            0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00,
        ];

        let (tx, rx) = tokio::sync::mpsc::channel(64);
        let cancel = CancellationToken::new();

        let cursor = std::io::Cursor::new(stream);
        read_and_parse_nals(cursor, tx, cancel);

        // Collect all NALs
        let mut nals = Vec::new();
        let mut rx = rx;
        while let Ok(msg) = rx.try_recv() {
            if let CaptureMessage::VideoNal(data) = msg {
                nals.push(data);
            }
        }

        assert_eq!(nals.len(), 3, "expected 3 NAL units, got {}", nals.len());
        // SPS NAL type
        assert_eq!(nals[0][0] & 0x1F, 7);
        // PPS NAL type
        assert_eq!(nals[1][0] & 0x1F, 8);
        // IDR NAL type
        assert_eq!(nals[2][0] & 0x1F, 5);
    }

    #[test]
    fn read_and_parse_handles_partial_reads() {
        // Simulate partial reads by using a reader that returns small chunks
        struct ChunkedReader {
            data: Vec<u8>,
            pos: usize,
            chunk_size: usize,
        }

        impl Read for ChunkedReader {
            fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
                if self.pos >= self.data.len() {
                    return Ok(0);
                }
                let end = (self.pos + self.chunk_size)
                    .min(self.data.len())
                    .min(self.pos + buf.len());
                let n = end - self.pos;
                buf[..n].copy_from_slice(&self.data[self.pos..end]);
                self.pos += n;
                Ok(n)
            }
        }

        let stream: Vec<u8> = vec![
            0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1e, 0x00, 0x00, 0x01, 0x68, 0xce, 0x38,
            0x80, 0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00,
        ];

        let (tx, rx) = tokio::sync::mpsc::channel(64);
        let cancel = CancellationToken::new();

        // Read 3 bytes at a time to exercise partial-read handling
        let reader = ChunkedReader {
            data: stream,
            pos: 0,
            chunk_size: 3,
        };
        read_and_parse_nals(reader, tx, cancel);

        let mut nals = Vec::new();
        let mut rx = rx;
        while let Ok(msg) = rx.try_recv() {
            if let CaptureMessage::VideoNal(data) = msg {
                nals.push(data);
            }
        }

        assert_eq!(nals.len(), 3, "expected 3 NAL units from chunked reads");
    }
}
