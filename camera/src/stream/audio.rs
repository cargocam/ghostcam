use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use anyhow::Result;
use ghostcam::wire::framing;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::capture::CaptureMessage;

/// Read Opus audio frames from the capture channel and write them to the
/// QUIC Audio stream as length-prefixed frames.
///
/// When `audio_enabled` is false, frames are consumed but not written.
pub async fn run_audio_writer(
    audio_tx: &mut quinn::SendStream,
    capture_rx: &mut mpsc::Receiver<CaptureMessage>,
    audio_enabled: Arc<AtomicBool>,
    cancel: CancellationToken,
) -> Result<()> {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            msg = capture_rx.recv() => {
                match msg {
                    Some(CaptureMessage::AudioFrame(data)) => {
                        if audio_enabled.load(Ordering::SeqCst) {
                            framing::write_frame(audio_tx, &data)
                                .await
                                .map_err(|e| anyhow::anyhow!("audio write error: {e}"))?;
                        }
                    }
                    Some(_) => {} // Skip non-audio messages
                    None => return Ok(()),
                }
            }
        }
    }
}
