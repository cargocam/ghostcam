use std::collections::HashMap;

use serde::{Deserialize, Serialize};

/// Firmware release metadata. Stored in-memory on the server, returned by
/// `GET /api/v1/firmware/latest`, and published via Redis pub/sub.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FirmwareRelease {
    pub version: String,
    pub assets: HashMap<String, FirmwareAsset>,
}

/// A single architecture's firmware binary.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FirmwareAsset {
    pub url: String,
    pub sha256: String,
}

/// Response from `GET /api/v1/firmware/latest`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FirmwareLatestResponse {
    pub version: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub assets: Option<HashMap<String, FirmwareAsset>>,
}

/// Compare two semver version strings. Returns true if `available` is newer
/// than `current`. Both may optionally have a leading "v" prefix.
///
/// Only compares major.minor.patch (ignores pre-release labels).
/// Returns false on parse errors (fail-safe: don't update if versions are garbled).
pub fn is_newer_version(current: &str, available: &str) -> bool {
    let parse = |s: &str| -> Option<(u64, u64, u64)> {
        let s = s.strip_prefix('v').unwrap_or(s);
        // Strip pre-release suffix (e.g., "1.2.3-rc1" -> "1.2.3")
        let s = s.split('-').next().unwrap_or(s);
        let parts: Vec<&str> = s.split('.').collect();
        if parts.len() != 3 {
            return None;
        }
        Some((
            parts[0].parse().ok()?,
            parts[1].parse().ok()?,
            parts[2].parse().ok()?,
        ))
    };

    let Some(cur) = parse(current) else {
        return false;
    };
    let Some(avail) = parse(available) else {
        return false;
    };

    avail > cur
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn newer_version_basic() {
        assert!(is_newer_version("0.1.0", "0.2.0"));
        assert!(is_newer_version("0.1.0", "1.0.0"));
        assert!(is_newer_version("1.2.3", "1.2.4"));
        assert!(is_newer_version("1.2.3", "1.3.0"));
        assert!(is_newer_version("1.2.3", "2.0.0"));
    }

    #[test]
    fn not_newer_same_or_older() {
        assert!(!is_newer_version("1.0.0", "1.0.0"));
        assert!(!is_newer_version("1.0.0", "0.9.0"));
        assert!(!is_newer_version("2.0.0", "1.99.99"));
    }

    #[test]
    fn v_prefix_stripped() {
        assert!(is_newer_version("v0.1.0", "v0.2.0"));
        assert!(is_newer_version("0.1.0", "v0.2.0"));
        assert!(is_newer_version("v0.1.0", "0.2.0"));
    }

    #[test]
    fn prerelease_stripped() {
        assert!(is_newer_version("1.0.0-rc1", "1.0.1"));
        assert!(is_newer_version("1.0.0", "1.0.1-beta"));
    }

    #[test]
    fn invalid_versions_return_false() {
        assert!(!is_newer_version("not-a-version", "1.0.0"));
        assert!(!is_newer_version("1.0.0", "not-a-version"));
        assert!(!is_newer_version("1.0", "1.0.1"));
        assert!(!is_newer_version("1.0.0", "1.0"));
    }

    #[test]
    fn firmware_latest_response_null_version() {
        let resp = FirmwareLatestResponse {
            version: None,
            assets: None,
        };
        let json = serde_json::to_string(&resp).unwrap();
        assert_eq!(json, r#"{"version":null}"#);
    }

    #[test]
    fn firmware_latest_response_with_version() {
        let mut assets = HashMap::new();
        assets.insert(
            "aarch64".to_string(),
            FirmwareAsset {
                url: "https://example.com/fw-aarch64".to_string(),
                sha256: "abc123".to_string(),
            },
        );
        let resp = FirmwareLatestResponse {
            version: Some("1.2.0".to_string()),
            assets: Some(assets),
        };
        let json = serde_json::to_string(&resp).unwrap();
        assert!(json.contains(r#""version":"1.2.0""#));
        assert!(json.contains("aarch64"));
    }

    #[test]
    fn firmware_release_roundtrip() {
        let mut assets = HashMap::new();
        assets.insert(
            "aarch64".to_string(),
            FirmwareAsset {
                url: "https://example.com/fw".to_string(),
                sha256: "deadbeef".to_string(),
            },
        );
        let release = FirmwareRelease {
            version: "1.0.0".to_string(),
            assets,
        };
        let json = serde_json::to_string(&release).unwrap();
        let back: FirmwareRelease = serde_json::from_str(&json).unwrap();
        assert_eq!(back.version, "1.0.0");
        assert!(back.assets.contains_key("aarch64"));
    }
}
