use std::path::Path;

use anyhow::Result;
use ghostcam::config::{env_opt, load_toml};
use serde::Deserialize;

/// Fully resolved camera configuration.
pub struct CameraConfig {
    pub server_addr: String,
    pub test_source: bool,
    pub test_video: String,
    pub segment_dir: String,
    pub no_audio: bool,
    /// Audio input device name (None = system default)
    pub audio_device: Option<String>,
    pub no_gps: bool,
    pub no_tofu: bool,
    pub data_dir: String,
    /// Video resolution width (default: 1280)
    pub video_width: u32,
    /// Video resolution height (default: 720)
    pub video_height: u32,
    /// Video framerate (default: 30)
    pub video_fps: u32,
    /// Video bitrate in bps (default: 2_000_000). 0 = VBR
    pub video_bitrate: u32,
    /// Keyframe interval in frames (default: 60 = 2s at 30fps)
    pub video_keyframe_interval: u32,
}

/// TOML-deserialized camera config file. All fields optional.
#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct CameraConfigFile {
    pub server_addr: Option<String>,
    pub test_source: Option<bool>,
    pub test_video: Option<String>,
    pub segment_dir: Option<String>,
    pub no_audio: Option<bool>,
    pub audio_device: Option<String>,
    pub no_gps: Option<bool>,
    /// Security flag — only settable via CLI, never from config file.
    #[serde(skip)]
    pub no_tofu: Option<bool>,
    pub data_dir: Option<String>,
    pub video_width: Option<u32>,
    pub video_height: Option<u32>,
    pub video_fps: Option<u32>,
    pub video_bitrate: Option<u32>,
    pub video_keyframe_interval: Option<u32>,
}

impl CameraConfig {
    /// Load configuration with layering: defaults -> config file -> env vars -> CLI flags.
    ///
    /// Config file search order:
    /// 1. `--config` CLI flag (if provided)
    /// 2. `$GHOSTCAM_CONFIG_FILE`
    /// 3. `$GHOSTCAM_DATA_DIR/camera.toml`
    /// 4. `/boot/ghostcam.conf` (backward compat — valid TOML key=value)
    pub fn load(cli: &super::Cli) -> Result<Self> {
        let file_conf = Self::find_and_load_config_file(cli)?;

        // Resolve data_dir: CLI -> env -> file -> default
        let data_dir = cli
            .data_dir
            .clone()
            .or_else(|| env_opt("GHOSTCAM_DATA_DIR"))
            .or(file_conf.data_dir)
            .unwrap_or_else(|| "/var/ghostcam".to_string());

        // Resolve server_addr: CLI -> env -> file -> stored addr file -> default
        let server_addr = if cli.server_addr.is_some() {
            cli.server_addr.clone().unwrap()
        } else {
            env_opt("GHOSTCAM_SERVER_ADDR")
                .or(file_conf.server_addr)
                .unwrap_or_else(|| {
                    // Check stored server.addr file (written during enrollment)
                    let addr_file = Path::new(&data_dir).join("server.addr");
                    if let Ok(addr) = std::fs::read_to_string(&addr_file) {
                        let addr = addr.trim();
                        if !addr.is_empty() {
                            return addr.to_string();
                        }
                    }
                    format!("127.0.0.1:{}", ghostcam::config::QUIC_PORT)
                })
        };

        let test_source = cli.test_source || file_conf.test_source.unwrap_or(false);

        let test_video = cli
            .test_video
            .clone()
            .or(file_conf.test_video)
            .unwrap_or_else(|| "test-data/test.h264".to_string());

        let no_audio = cli.no_audio || file_conf.no_audio.unwrap_or(false);

        let audio_device = env_opt("GHOSTCAM_AUDIO_DEVICE").or(file_conf.audio_device);

        let no_gps = cli.no_gps || file_conf.no_gps.unwrap_or(false);

        // no_tofu is CLI-only (intentional security decision — never from config file)
        let no_tofu = cli.no_tofu;

        let segment_dir = cli
            .segment_dir
            .clone()
            .or_else(|| env_opt("GHOSTCAM_SEGMENT_DIR"))
            .or(file_conf.segment_dir)
            .unwrap_or_else(|| format!("{data_dir}/segments"));

        // Video profile presets: applied between config file and individual env vars
        let (profile_width, profile_height, profile_bitrate) =
            match env_opt("GHOSTCAM_VIDEO_PROFILE").as_deref() {
                Some("zero2w" | "480p") => {
                    tracing::info!(profile = "zero2w/480p", "applying video profile");
                    (Some(854u32), Some(480u32), Some(1_000_000u32))
                }
                Some("pi4" | "720p") => {
                    tracing::info!(profile = "pi4/720p", "applying video profile");
                    (Some(1280), Some(720), Some(2_000_000))
                }
                Some("pi5" | "1080p") => {
                    tracing::info!(profile = "pi5/1080p", "applying video profile");
                    (Some(1920), Some(1080), Some(4_000_000))
                }
                Some(other) => {
                    tracing::warn!(profile = other, "unknown video profile, ignoring");
                    (None, None, None)
                }
                None => (None, None, None),
            };

        let video_width = env_opt("GHOSTCAM_VIDEO_WIDTH")
            .and_then(|s| s.parse().ok())
            .or(profile_width)
            .or(file_conf.video_width)
            .unwrap_or(1280);

        let video_height = env_opt("GHOSTCAM_VIDEO_HEIGHT")
            .and_then(|s| s.parse().ok())
            .or(profile_height)
            .or(file_conf.video_height)
            .unwrap_or(720);

        let video_fps = env_opt("GHOSTCAM_VIDEO_FPS")
            .and_then(|s| s.parse().ok())
            .or(file_conf.video_fps)
            .unwrap_or(30);

        let video_bitrate = env_opt("GHOSTCAM_VIDEO_BITRATE")
            .and_then(|s| s.parse().ok())
            .or(profile_bitrate)
            .or(file_conf.video_bitrate)
            .unwrap_or(2_000_000);

        let video_keyframe_interval = env_opt("GHOSTCAM_VIDEO_KEYFRAME_INTERVAL")
            .and_then(|s| s.parse().ok())
            .or(file_conf.video_keyframe_interval)
            .unwrap_or(60);

        let config = CameraConfig {
            server_addr,
            test_source,
            test_video,
            segment_dir,
            no_audio,
            audio_device,
            no_gps,
            no_tofu,
            data_dir,
            video_width,
            video_height,
            video_fps,
            video_bitrate,
            video_keyframe_interval,
        };

        config.validate()?;
        Ok(config)
    }

    fn find_and_load_config_file(cli: &super::Cli) -> Result<CameraConfigFile> {
        // Build candidate list
        let data_dir_env = env_opt("GHOSTCAM_DATA_DIR");
        let candidates: Vec<std::path::PathBuf> = [
            cli.config.clone(),
            env_opt("GHOSTCAM_CONFIG_FILE"),
            data_dir_env.map(|d| format!("{d}/camera.toml")),
            Some("/boot/ghostcam.conf".to_string()),
        ]
        .into_iter()
        .flatten()
        .map(std::path::PathBuf::from)
        .collect();

        for path in &candidates {
            if path.exists() {
                tracing::info!(path = %path.display(), "loading config file");
                return load_toml(path);
            }
        }

        Ok(CameraConfigFile::default())
    }

    fn validate(&self) -> Result<()> {
        if self.server_addr.is_empty() {
            anyhow::bail!("server address must not be empty");
        }
        if self.data_dir.is_empty() {
            anyhow::bail!("data directory must not be empty");
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn load_toml_camera_config() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("camera.toml");
        let mut f = std::fs::File::create(&path).unwrap();
        writeln!(f, "server_addr = \"10.0.0.1:4433\"").unwrap();
        writeln!(f, "no_audio = true").unwrap();
        writeln!(f, "no_gps = true").unwrap();

        let conf: CameraConfigFile = load_toml(&path).unwrap();
        assert_eq!(conf.server_addr.as_deref(), Some("10.0.0.1:4433"));
        assert_eq!(conf.no_audio, Some(true));
        assert_eq!(conf.no_gps, Some(true));
    }

    #[test]
    fn load_toml_backward_compat_ghostcam_conf() {
        // /boot/ghostcam.conf uses key = "value" format, which is valid TOML
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ghostcam.conf");
        let mut f = std::fs::File::create(&path).unwrap();
        writeln!(f, "server_addr = \"10.0.0.1:4433\"").unwrap();
        writeln!(f, "no_audio = true").unwrap();

        let conf: CameraConfigFile = load_toml(&path).unwrap();
        assert_eq!(conf.server_addr.as_deref(), Some("10.0.0.1:4433"));
        assert_eq!(conf.no_audio, Some(true));
        assert!(!conf.no_gps.unwrap_or(false));
    }

    #[test]
    fn load_toml_empty_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("camera.toml");
        std::fs::File::create(&path).unwrap();

        let conf: CameraConfigFile = load_toml(&path).unwrap();
        assert!(conf.server_addr.is_none());
        assert!(!conf.no_audio.unwrap_or(false));
    }
}
