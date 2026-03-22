use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use anyhow::Result;
use ghostcam::wire::framing;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::capture::CaptureMessage;

/// Read video NAL units from the capture channel and write them to the
/// QUIC Video stream as length-prefixed frames.
///
/// When `video_enabled` is false, frames are consumed but not written,
/// keeping the channel drained.
pub async fn run_video_writer(
    video_tx: &mut quinn::SendStream,
    capture_rx: &mut mpsc::Receiver<CaptureMessage>,
    video_enabled: Arc<AtomicBool>,
    cancel: CancellationToken,
) -> Result<()> {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            msg = capture_rx.recv() => {
                match msg {
                    Some(CaptureMessage::VideoNal(data)) => {
                        if video_enabled.load(Ordering::SeqCst) {
                            framing::write_frame(video_tx, &data)
                                .await
                                .map_err(|e| anyhow::anyhow!("video write error: {e}"))?;
                        }
                    }
                    Some(_) => {} // Skip non-video messages
                    None => return Ok(()), // Channel closed
                }
            }
        }
    }
}
