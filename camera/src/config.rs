use std::path::Path;

use anyhow::Result;
use serde::Deserialize;

/// Parsed camera configuration from all sources.
pub struct CameraConfig {
    pub server_addr: String,
    pub test_source: bool,
    pub test_video: String,
    pub no_audio: bool,
    pub no_gps: bool,
    pub no_tofu: bool,
    pub data_dir: String,
}

/// Configuration from /boot/ghostcam.conf (TOML subset, hand-parsed).
#[derive(Debug, Default, Deserialize)]
pub struct GhostcamConf {
    pub server_addr: Option<String>,
    #[serde(default)]
    pub no_audio: bool,
    #[serde(default)]
    pub no_gps: bool,
}

/// Parse ghostcam.conf from the boot partition.
/// Returns None if the file does not exist.
pub fn read_ghostcam_conf(path: &Path) -> Result<Option<GhostcamConf>> {
    if !path.exists() {
        return Ok(None);
    }
    let contents = std::fs::read_to_string(path)?;
    // Simple key=value parser (subset of TOML, no sections or complex types)
    let mut conf = GhostcamConf::default();
    for line in contents.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((key, value)) = line.split_once('=') {
            let key = key.trim();
            let value = value.trim().trim_matches('"');
            match key {
                "server_addr" => conf.server_addr = Some(value.to_string()),
                "no_audio" => conf.no_audio = value == "true",
                "no_gps" => conf.no_gps = value == "true",
                _ => {}
            }
        }
    }
    Ok(Some(conf))
}

/// Resolve server address with precedence:
/// 1. CLI --server-addr
/// 2. ghostcam.conf server_addr
/// 3. /etc/ghostcam/server.addr (stored during enrollment)
/// 4. Hardcoded default
pub fn resolve_server_addr(cli: Option<&str>, conf: Option<&str>, addr_file: &Path) -> String {
    if let Some(addr) = cli {
        return addr.to_string();
    }
    if let Some(addr) = conf {
        return addr.to_string();
    }
    if let Ok(addr) = std::fs::read_to_string(addr_file) {
        let addr = addr.trim();
        if !addr.is_empty() {
            return addr.to_string();
        }
    }
    format!("127.0.0.1:{}", ghostcam::config::QUIC_PORT)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn parse_ghostcam_conf_full() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ghostcam.conf");
        let mut f = std::fs::File::create(&path).unwrap();
        writeln!(f, "server_addr = \"10.0.0.1:4433\"").unwrap();
        writeln!(f, "no_audio = true").unwrap();
        writeln!(f, "no_gps = true").unwrap();

        let conf = read_ghostcam_conf(&path).unwrap().unwrap();
        assert_eq!(conf.server_addr.as_deref(), Some("10.0.0.1:4433"));
        assert!(conf.no_audio);
        assert!(conf.no_gps);
    }

    #[test]
    fn parse_ghostcam_conf_minimal() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ghostcam.conf");
        let mut f = std::fs::File::create(&path).unwrap();
        writeln!(f, "server_addr = \"10.0.0.1:4433\"").unwrap();

        let conf = read_ghostcam_conf(&path).unwrap().unwrap();
        assert_eq!(conf.server_addr.as_deref(), Some("10.0.0.1:4433"));
        assert!(!conf.no_audio);
        assert!(!conf.no_gps);
    }

    #[test]
    fn parse_ghostcam_conf_empty() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ghostcam.conf");
        std::fs::File::create(&path).unwrap();

        let conf = read_ghostcam_conf(&path).unwrap().unwrap();
        assert!(conf.server_addr.is_none());
        assert!(!conf.no_audio);
    }

    #[test]
    fn parse_ghostcam_conf_missing_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nonexistent.conf");
        assert!(read_ghostcam_conf(&path).unwrap().is_none());
    }

    #[test]
    fn resolve_addr_cli_wins() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("server.addr");
        std::fs::write(&file, "from-file:4433").unwrap();
        let addr = resolve_server_addr(Some("from-cli:4433"), Some("from-conf:4433"), &file);
        assert_eq!(addr, "from-cli:4433");
    }

    #[test]
    fn resolve_addr_conf_second() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("server.addr");
        std::fs::write(&file, "from-file:4433").unwrap();
        let addr = resolve_server_addr(None, Some("from-conf:4433"), &file);
        assert_eq!(addr, "from-conf:4433");
    }

    #[test]
    fn resolve_addr_file_third() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("server.addr");
        std::fs::write(&file, "from-file:4433").unwrap();
        let addr = resolve_server_addr(None, None, &file);
        assert_eq!(addr, "from-file:4433");
    }

    #[test]
    fn resolve_addr_default() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("nonexistent");
        let addr = resolve_server_addr(None, None, &file);
        assert_eq!(addr, format!("127.0.0.1:{}", ghostcam::config::QUIC_PORT));
    }
}
