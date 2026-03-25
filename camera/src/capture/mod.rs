pub mod audio_test;
pub mod video_test;

use bytes::Bytes;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::config::CameraConfig;

/// Messages produced by the capture pipeline.
#[derive(Debug, Clone)]
pub enum CaptureMessage {
    /// H.264 NAL unit(s) from video capture.
    VideoNal(Bytes),
    /// Opus-encoded audio frame.
    AudioFrame(Bytes),
}

pub type CaptureSender = mpsc::Sender<CaptureMessage>;
pub type CaptureReceiver = mpsc::Receiver<CaptureMessage>;

/// Start the capture pipeline. Returns a receiver for capture messages.
pub async fn start_capture(
    config: &CameraConfig,
    cancel: CancellationToken,
) -> anyhow::Result<CaptureReceiver> {
    let (tx, rx) = mpsc::channel(256);

    if config.test_source {
        // Test video source
        let video_tx = tx.clone();
        let video_path = config.test_video.clone();
        let video_cancel = cancel.clone();
        tokio::spawn(async move {
            if let Err(e) = video_test::run_test_video(&video_path, video_tx, video_cancel).await {
                tracing::warn!("test video source ended: {e}");
            }
        });

        // Test audio source
        if !config.no_audio {
            let audio_tx = tx;
            let audio_cancel = cancel;
            tokio::spawn(async move {
                if let Err(e) = audio_test::run_test_audio(audio_tx, audio_cancel).await {
                    tracing::warn!("test audio source ended: {e}");
                }
            });
        }
    } else {
        // Real capture — stub for Plan 8/9
        // On Linux: rpicam-vid for video, cpal+opus for audio
        // On non-Linux: fall back to test sources
        tracing::warn!("real capture not yet implemented, using test sources");
        let video_tx = tx.clone();
        let video_path = config.test_video.clone();
        let video_cancel = cancel.clone();
        tokio::spawn(async move {
            if let Err(e) = video_test::run_test_video(&video_path, video_tx, video_cancel).await {
                tracing::warn!("test video source ended: {e}");
            }
        });

        if !config.no_audio {
            let audio_tx = tx;
            let audio_cancel = cancel;
            tokio::spawn(async move {
                if let Err(e) = audio_test::run_test_audio(audio_tx, audio_cancel).await {
                    tracing::warn!("test audio source ended: {e}");
                }
            });
        }
    }

    Ok(rx)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_audio_produces_frames() {
        let (tx, mut rx) = mpsc::channel(64);
        let cancel = CancellationToken::new();
        let c = cancel.clone();

        tokio::spawn(async move {
            let _ = audio_test::run_test_audio(tx, c).await;
        });

        // Should receive frames within 100ms
        let frame = tokio::time::timeout(std::time::Duration::from_millis(100), rx.recv())
            .await
            .expect("timeout waiting for audio frame")
            .expect("channel closed");

        cancel.cancel();

        match frame {
            CaptureMessage::AudioFrame(data) => {
                // Real Opus tone frame (80 bytes, first byte 0x78)
                assert_eq!(data.len(), 80);
                assert_eq!(data[0], 0x78);
            }
            _ => panic!("expected AudioFrame"),
        }
    }
}
