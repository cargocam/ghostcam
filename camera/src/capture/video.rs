use anyhow::{bail, Context, Result};
use ghostcam::h264::NalParser;
use std::process::Stdio;
use tokio::io::AsyncReadExt;
use tokio::sync::mpsc;
use tracing::{info, warn};

use super::CaptureMessage;

#[derive(Debug, Clone)]
pub struct VideoCaptureConfig {
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    /// Bitrate in bits/s. 0 = let rpicam-vid decide.
    pub bitrate: u32,
    /// Keyframe interval in frames.
    pub keyframe_interval: u32,
}

impl Default for VideoCaptureConfig {
    fn default() -> Self {
        Self {
            width: 1280,
            height: 720,
            fps: 30,
            bitrate: 0,
            keyframe_interval: 60,
        }
    }
}

pub struct VideoCapture {
    child: tokio::process::Child,
}

impl VideoCapture {
    /// Start video capture, spawning rpicam-vid (or libcamera-vid) as a subprocess.
    /// NAL units are sent on `tx` as `CaptureMessage::VideoNal`.
    pub async fn start(
        config: VideoCaptureConfig,
        tx: mpsc::Sender<CaptureMessage>,
    ) -> Result<Self> {
        let cmd_name = detect_camera_command().await?;
        info!(command = %cmd_name, "detected camera command");

        let mut args = vec![
            "-t".to_string(),
            "0".to_string(),
            "--width".to_string(),
            config.width.to_string(),
            "--height".to_string(),
            config.height.to_string(),
            "--framerate".to_string(),
            config.fps.to_string(),
            "--codec".to_string(),
            "h264".to_string(),
            "-o".to_string(),
            "-".to_string(),
            "--flush".to_string(),
            "-n".to_string(),
            "--inline".to_string(),
            "--intra".to_string(),
            config.keyframe_interval.to_string(),
        ];

        if config.bitrate > 0 {
            args.push("--bitrate".to_string());
            args.push(config.bitrate.to_string());
        }

        info!(cmd = %cmd_name, args = ?args, "launching camera process");

        let mut child = tokio::process::Command::new(&cmd_name)
            .args(&args)
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .context("failed to spawn camera process")?;

        let stdout = child.stdout.take().expect("stdout piped");

        tokio::spawn(async move {
            if let Err(e) = read_nal_stream(stdout, tx).await {
                warn!(error = %e, "video capture stream ended");
            }
        });

        Ok(Self { child })
    }

    /// Kill the camera subprocess.
    pub async fn stop(&mut self) {
        let _ = self.child.kill().await;
    }
}

impl Drop for VideoCapture {
    fn drop(&mut self) {
        // Best-effort kill — start_kill is non-blocking
        let _ = self.child.start_kill();
    }
}

/// Detect whether rpicam-vid or libcamera-vid is available.
async fn detect_camera_command() -> Result<String> {
    for cmd in &["rpicam-vid", "libcamera-vid"] {
        let result = tokio::process::Command::new(cmd)
            .arg("--help")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .await;
        if result.is_ok() {
            return Ok(cmd.to_string());
        }
    }
    bail!("neither rpicam-vid nor libcamera-vid found in PATH")
}

/// Read stdout from camera process, parse NAL units, send on channel.
async fn read_nal_stream(
    mut stdout: tokio::process::ChildStdout,
    tx: mpsc::Sender<CaptureMessage>,
) -> Result<()> {
    let mut parser = NalParser::new();
    let mut buf = vec![0u8; 64 * 1024];

    loop {
        let n = stdout.read(&mut buf).await?;
        if n == 0 {
            // EOF — camera process exited
            if let Some(nal) = parser.flush() {
                let nal_type = if nal.is_empty() { 0 } else { nal[0] & 0x1F };
                let _ = tx
                    .send(CaptureMessage::VideoNal {
                        nal_data: nal,
                        nal_type,
                    })
                    .await;
            }
            bail!("camera process stdout closed");
        }

        let nals = parser.feed(&buf[..n]);
        for nal in nals {
            let nal_type = if nal.is_empty() { 0 } else { nal[0] & 0x1F };
            if tx
                .send(CaptureMessage::VideoNal {
                    nal_data: nal,
                    nal_type,
                })
                .await
                .is_err()
            {
                return Ok(()); // receiver dropped
            }
        }
    }
}
