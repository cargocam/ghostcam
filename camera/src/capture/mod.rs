#[cfg(target_os = "linux")]
pub mod audio;
pub mod audio_test;
pub mod video;
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
        // Real video capture via rpicam-vid / libcamera-vid.
        // Verify the binary exists before spawning — fail loudly, never fall back silently.
        if video::detect_capture_binary().await.is_none() {
            anyhow::bail!(
                "real video capture requested but neither rpicam-vid nor libcamera-vid \
                 found on PATH. Install rpicam-apps or use --test-source for development."
            );
        }

        let video_tx = tx.clone();
        let video_cancel = cancel.clone();
        // Clone config fields needed by the spawned task
        let video_width = config.video_width;
        let video_height = config.video_height;
        let video_fps = config.video_fps;
        let video_bitrate = config.video_bitrate;
        let video_keyframe_interval = config.video_keyframe_interval;
        tokio::spawn(async move {
            // Build a minimal config for the video task
            let video_config = CameraConfig {
                server_addr: String::new(),
                test_source: false,
                test_video: String::new(),
                segment_dir: String::new(),
                no_audio: true,
                audio_device: None,
                no_gps: true,
                no_tofu: true,
                data_dir: String::new(),
                video_width,
                video_height,
                video_fps,
                video_bitrate,
                video_keyframe_interval,
            };
            if let Err(e) = video::run_real_video(&video_config, video_tx, video_cancel).await {
                tracing::error!("real video capture failed: {e}");
            }
        });

        // Real audio capture on Linux, test audio fallback on other platforms.
        if !config.no_audio {
            start_audio_capture(config, tx, cancel);
        }
    }

    Ok(rx)
}

/// Start audio capture: real cpal+opus on Linux, test source on other platforms.
fn start_audio_capture(config: &CameraConfig, tx: CaptureSender, cancel: CancellationToken) {
    #[cfg(target_os = "linux")]
    {
        let device_name = config.audio_device.clone();
        let audio_tx = tx.clone();
        let audio_cancel = cancel.clone();

        match audio::start_real_audio(device_name.as_deref(), audio_tx, audio_cancel) {
            Ok(()) => {
                tracing::info!("real audio capture started");
                return;
            }
            Err(e) => {
                tracing::warn!("real audio capture failed to initialize: {e}");
                tracing::warn!("falling back to test audio source");
            }
        }
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = &config.audio_device; // suppress unused warning
    }

    // Fallback: test audio source (non-Linux or if real audio init failed)
    let audio_tx = tx;
    let audio_cancel = cancel;
    tokio::spawn(async move {
        if let Err(e) = audio_test::run_test_audio(audio_tx, audio_cancel).await {
            tracing::warn!("test audio source ended: {e}");
        }
    });
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
