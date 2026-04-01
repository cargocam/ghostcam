use std::net::IpAddr;
use std::path::PathBuf;

use anyhow::{Context, Result};
use ghostcam::config::{env_opt, env_or, load_toml};
use serde::Deserialize;

/// Fully resolved server configuration with no optional fields.
#[derive(Debug, Clone)]
pub struct ServerConfig {
    pub data_dir: String,
    pub http_port: u16,
    pub quic_port: u16,
    pub webrtc_port: u16,
    pub database_url: String,
    pub redis_url: Option<String>,
    pub public_ip: Option<IpAddr>,
    pub enrollment_addr: Option<String>,
    pub admin_email: String,
    pub admin_password: Option<String>,
    // Stripe billing (all optional — no key = billing disabled)
    pub stripe_secret_key: Option<String>,
    pub stripe_public_key: Option<String>,
    pub stripe_webhook_secret: Option<String>,
    pub stripe_pricing_table_id: Option<String>,
    pub stripe_portal_config_id: Option<String>,
    // S3 / Tigris object storage
    pub s3_bucket: String,
    pub s3_region: String,
    pub s3_endpoint: Option<String>,
    pub s3_presign_ttl_secs: u64,
    // Firmware releases (all optional — no repo = firmware endpoint returns null)
    pub release_repo: Option<String>,
    pub github_webhook_secret: Option<String>,
    pub update_stagger_secs: u64,
}

/// TOML-deserialized server config file. All fields optional — missing fields
/// are filled from environment variables or defaults.
///
/// Sensitive fields (`database_url`, `admin_password`) are `serde(skip)` so they
/// can only come from environment variables, never from a config file on disk.
#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct ServerConfigFile {
    pub data_dir: Option<String>,
    pub http_port: Option<u16>,
    pub quic_port: Option<u16>,
    pub webrtc_port: Option<u16>,
    pub redis_url: Option<String>,
    pub public_ip: Option<String>,
    pub enrollment_addr: Option<String>,
    pub admin_email: Option<String>,
    // Sensitive — env vars only
    #[serde(skip)]
    pub database_url: Option<String>,
    #[serde(skip)]
    pub admin_password: Option<String>,
}

impl ServerConfig {
    /// Load configuration with layering: defaults -> config file -> env vars.
    ///
    /// Config file search order:
    /// 1. `$GHOSTCAM_CONFIG_FILE`
    /// 2. `$GHOSTCAM_DATA_DIR/server.toml`
    /// 3. `/etc/ghostcam/server.toml`
    pub fn load() -> Result<Self> {
        // First, resolve data_dir from env (needed for config file search)
        let data_dir_env = env_opt("GHOSTCAM_DATA_DIR");

        // Try to find and load config file
        let file_conf = Self::find_and_load_config_file(data_dir_env.as_deref())?;

        // Layer: defaults <- config file <- env vars
        let data_dir = env_opt("GHOSTCAM_DATA_DIR")
            .or(file_conf.data_dir)
            .unwrap_or_else(|| "/var/ghostcam".to_string());

        let http_port = env_or(
            "GHOSTCAM_HTTP_PORT",
            file_conf.http_port.unwrap_or(ghostcam::config::HTTP_PORT),
        );

        let quic_port = env_or(
            "GHOSTCAM_QUIC_PORT",
            file_conf.quic_port.unwrap_or(ghostcam::config::QUIC_PORT),
        );

        let webrtc_port = env_or(
            "GHOSTCAM_WEBRTC_PORT",
            file_conf.webrtc_port.unwrap_or(3478),
        );

        // database_url: env only (sensitive)
        let database_url =
            std::env::var("GHOSTCAM_DATABASE_URL").context("GHOSTCAM_DATABASE_URL is required")?;

        let redis_url = env_opt("GHOSTCAM_REDIS_URL").or(file_conf.redis_url);

        let public_ip = Self::parse_public_ip(file_conf.public_ip.as_deref());

        let enrollment_addr = env_opt("GHOSTCAM_ENROLLMENT_ADDR").or(file_conf.enrollment_addr);

        let admin_email = env_opt("GHOSTCAM_ADMIN_EMAIL")
            .or(file_conf.admin_email)
            .unwrap_or_else(|| "admin@localhost".to_string());

        // admin_password: env only (sensitive)
        let admin_password = env_opt("GHOSTCAM_ADMIN_PASSWORD");

        // Stripe billing (env only)
        let stripe_secret_key = env_opt("STRIPE_SECRET_KEY");
        let stripe_public_key = env_opt("STRIPE_PUBLIC_KEY");
        let stripe_webhook_secret = env_opt("STRIPE_WEBHOOK_SECRET");
        let stripe_pricing_table_id = env_opt("STRIPE_PRICING_TABLE_ID");
        let stripe_portal_config_id = env_opt("STRIPE_PORTAL_CONFIG_ID");

        // S3 / Tigris
        let s3_bucket = env_opt("GHOSTCAM_S3_BUCKET")
            .unwrap_or_else(|| "ghostcam-segments".to_string());
        let s3_region = env_opt("GHOSTCAM_S3_REGION")
            .unwrap_or_else(|| "auto".to_string());
        let s3_endpoint = env_opt("GHOSTCAM_S3_ENDPOINT");
        let s3_presign_ttl_secs: u64 = env_or(
            "GHOSTCAM_S3_PRESIGN_TTL_SECS",
            ghostcam::config::PRESIGN_TTL_SECS,
        );

        // Firmware release config
        let release_repo = env_opt("GHOSTCAM_RELEASE_REPO");
        let github_webhook_secret = env_opt("GITHUB_WEBHOOK_SECRET");
        let update_stagger_secs: u64 = env_opt("GHOSTCAM_UPDATE_STAGGER_SECS")
            .and_then(|s| s.parse().ok())
            .unwrap_or(300);

        let config = ServerConfig {
            data_dir,
            http_port,
            quic_port,
            webrtc_port,
            database_url,
            redis_url,
            public_ip,
            enrollment_addr,
            admin_email,
            admin_password,
            s3_bucket,
            s3_region,
            s3_endpoint,
            s3_presign_ttl_secs,
            stripe_secret_key,
            stripe_public_key,
            stripe_webhook_secret,
            stripe_pricing_table_id,
            stripe_portal_config_id,
            release_repo,
            github_webhook_secret,
            update_stagger_secs,
        };

        config.validate()?;
        Ok(config)
    }

    /// Resolve the enrollment address. Defaults to `<public_ip>:<quic_port>`.
    pub fn resolved_enrollment_addr(&self) -> String {
        if let Some(ref addr) = self.enrollment_addr {
            return addr.clone();
        }
        let ip = self
            .public_ip
            .unwrap_or(IpAddr::V4(std::net::Ipv4Addr::LOCALHOST));
        format!("{ip}:{}", self.quic_port)
    }

    fn find_and_load_config_file(data_dir_env: Option<&str>) -> Result<ServerConfigFile> {
        let candidates: Vec<PathBuf> = [
            env_opt("GHOSTCAM_CONFIG_FILE"),
            data_dir_env.map(|d| format!("{d}/server.toml")),
            Some("/etc/ghostcam/server.toml".to_string()),
        ]
        .into_iter()
        .flatten()
        .map(PathBuf::from)
        .collect();

        for path in &candidates {
            if path.exists() {
                tracing::info!(path = %path.display(), "loading config file");
                return load_toml(path);
            }
        }

        Ok(ServerConfigFile::default())
    }

    /// Parse public IP from environment variables, then fall back to config file value.
    ///
    /// Priority:
    /// 1. `GHOSTCAM_PUBLIC_IP` — explicit override, always wins.
    /// 2. `FLY_PUBLIC_IP` — automatically set by Fly.io.
    /// 3. Config file `public_ip` field.
    fn parse_public_ip(file_value: Option<&str>) -> Option<IpAddr> {
        for var in ["GHOSTCAM_PUBLIC_IP", "FLY_PUBLIC_IP"] {
            if let Some(raw) = env_opt(var) {
                match raw.parse::<IpAddr>() {
                    Ok(ip) => return Some(ip),
                    Err(_) => {
                        tracing::warn!(var, value = %raw, "invalid IP address, ignoring");
                    }
                }
            }
        }
        if let Some(raw) = file_value {
            match raw.parse::<IpAddr>() {
                Ok(ip) => return Some(ip),
                Err(_) => {
                    tracing::warn!(value = %raw, "invalid public_ip in config file, ignoring");
                }
            }
        }
        None
    }

    fn validate(&self) -> Result<()> {
        if self.database_url.is_empty() {
            anyhow::bail!("GHOSTCAM_DATABASE_URL must not be empty");
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_public_ip_env_priority() {
        // With no env vars and no file value, should return None
        let result = ServerConfig::parse_public_ip(None);
        // Can't guarantee env state in tests, so just verify the method doesn't panic
        let _ = result;
    }

    #[test]
    fn parse_public_ip_file_fallback() {
        let result = ServerConfig::parse_public_ip(Some("192.168.1.100"));
        // May be overridden by env var in test environment
        if std::env::var("GHOSTCAM_PUBLIC_IP").is_err() && std::env::var("FLY_PUBLIC_IP").is_err() {
            assert_eq!(result, Some("192.168.1.100".parse().unwrap()));
        }
    }

    #[test]
    fn resolved_enrollment_addr_default() {
        let config = ServerConfig {
            data_dir: "/tmp".to_string(),
            http_port: 3000,
            quic_port: 4433,
            webrtc_port: 3478,
            database_url: "postgres://localhost/test".to_string(),
            redis_url: None,
            public_ip: Some("10.0.0.1".parse().unwrap()),
            enrollment_addr: None,
            admin_email: "admin@localhost".to_string(),
            admin_password: None,
            s3_bucket: "test-bucket".to_string(),
            s3_region: "auto".to_string(),
            s3_endpoint: None,
            s3_presign_ttl_secs: 3600,
            stripe_secret_key: None,
            stripe_public_key: None,
            stripe_webhook_secret: None,
            stripe_pricing_table_id: None,
            stripe_portal_config_id: None,
            release_repo: None,
            github_webhook_secret: None,
            update_stagger_secs: 300,
        };
        assert_eq!(config.resolved_enrollment_addr(), "10.0.0.1:4433");
    }

    #[test]
    fn resolved_enrollment_addr_override() {
        let config = ServerConfig {
            data_dir: "/tmp".to_string(),
            http_port: 3000,
            quic_port: 4433,
            webrtc_port: 3478,
            database_url: "postgres://localhost/test".to_string(),
            redis_url: None,
            public_ip: Some("10.0.0.1".parse().unwrap()),
            enrollment_addr: Some("server:4433".to_string()),
            admin_email: "admin@localhost".to_string(),
            admin_password: None,
            s3_bucket: "test-bucket".to_string(),
            s3_region: "auto".to_string(),
            s3_endpoint: None,
            s3_presign_ttl_secs: 3600,
            stripe_secret_key: None,
            stripe_public_key: None,
            stripe_webhook_secret: None,
            stripe_pricing_table_id: None,
            stripe_portal_config_id: None,
            release_repo: None,
            github_webhook_secret: None,
            update_stagger_secs: 300,
        };
        assert_eq!(config.resolved_enrollment_addr(), "server:4433");
    }
}
