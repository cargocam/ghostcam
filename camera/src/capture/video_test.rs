use std::time::Duration;

use anyhow::{Context, Result};
use bytes::Bytes;
use tokio_util::sync::CancellationToken;

use super::{CaptureMessage, CaptureSender};

/// Loop a test H.264 file, emitting NAL units at ~30fps.
pub async fn run_test_video(
    path: &str,
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()> {
    let data = tokio::fs::read(path)
        .await
        .with_context(|| format!("reading test video file: {path}"))?;

    let nals = parse_annex_b_nals(&data);
    if nals.is_empty() {
        anyhow::bail!("no NAL units found in {path}");
    }

    tracing::info!(path, nal_count = nals.len(), "test video source started");

    let frame_interval = Duration::from_micros(33_333); // ~30fps

    loop {
        for nal in &nals {
            if cancel.is_cancelled() {
                return Ok(());
            }

            let msg = CaptureMessage::VideoNal(Bytes::copy_from_slice(nal));
            if tx.send(msg).await.is_err() {
                return Ok(());
            }

            tokio::select! {
                _ = cancel.cancelled() => return Ok(()),
                _ = tokio::time::sleep(frame_interval) => {}
            }
        }
    }
}

/// Parse Annex-B formatted H.264 data into individual NAL units.
fn parse_annex_b_nals(data: &[u8]) -> Vec<&[u8]> {
    let mut nals = Vec::new();
    let mut i = 0;

    // Find first start code
    while i < data.len() {
        if i + 3 <= data.len() && data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1 {
            i += 3;
            break;
        }
        if i + 4 <= data.len()
            && data[i] == 0
            && data[i + 1] == 0
            && data[i + 2] == 0
            && data[i + 3] == 1
        {
            i += 4;
            break;
        }
        i += 1;
    }

    let mut nal_start = i;

    while i < data.len() {
        // Look for next start code
        if i + 3 <= data.len() && data[i] == 0 && data[i + 1] == 0 {
            if data[i + 2] == 1 {
                // 3-byte start code
                if nal_start < i {
                    nals.push(&data[nal_start..i]);
                }
                i += 3;
                nal_start = i;
                continue;
            }
            if i + 4 <= data.len() && data[i + 2] == 0 && data[i + 3] == 1 {
                // 4-byte start code
                if nal_start < i {
                    nals.push(&data[nal_start..i]);
                }
                i += 4;
                nal_start = i;
                continue;
            }
        }
        i += 1;
    }

    // Last NAL
    if nal_start < data.len() {
        nals.push(&data[nal_start..]);
    }

    nals
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_annex_b_basic() {
        let data = [
            0x00, 0x00, 0x00, 0x01, 0x67, 0xAA, // SPS
            0x00, 0x00, 0x01, 0x68, 0xBB, // PPS (3-byte start code)
            0x00, 0x00, 0x00, 0x01, 0x65, 0xCC, 0xDD, // IDR
        ];
        let nals = parse_annex_b_nals(&data);
        assert_eq!(nals.len(), 3);
        assert_eq!(nals[0], &[0x67, 0xAA]);
        assert_eq!(nals[1], &[0x68, 0xBB]);
        assert_eq!(nals[2], &[0x65, 0xCC, 0xDD]);
    }

    #[test]
    fn parse_annex_b_empty() {
        let nals = parse_annex_b_nals(&[]);
        assert!(nals.is_empty());
    }

    #[test]
    fn parse_annex_b_no_start_code() {
        let data = [0x67, 0xAA, 0xBB];
        let nals = parse_annex_b_nals(&data);
        assert!(nals.is_empty());
    }
}
