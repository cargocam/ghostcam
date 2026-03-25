use std::time::Duration;

use anyhow::Result;
use bytes::Bytes;
use tokio_util::sync::CancellationToken;

use super::{CaptureMessage, CaptureSender};

mod opus_tone_data;

/// Send pre-encoded Opus frames (440Hz + 880Hz sine tone) every 20ms, looping.
pub async fn run_test_audio(tx: CaptureSender, cancel: CancellationToken) -> Result<()> {
    tracing::info!(
        "test audio source started (Opus tone @ 50fps, {} frames looping)",
        opus_tone_data::TONE_FRAMES.len()
    );
    let interval = Duration::from_millis(20);

    let mut idx = 0;
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            _ = tokio::time::sleep(interval) => {
                let frame_data = opus_tone_data::TONE_FRAMES[idx];
                let msg = CaptureMessage::AudioFrame(Bytes::from_static(frame_data));
                if tx.send(msg).await.is_err() {
                    return Ok(());
                }
                idx = (idx + 1) % opus_tone_data::TONE_FRAMES.len();
            }
        }
    }
}
