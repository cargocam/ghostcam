use serde::{Deserialize, Serialize};

/// JWT claims for an enrollment token.
///
/// Claim tokens are stateless JWTs signed by the server's CA key.
/// The `sub` field carries the owning user_id so the server can assign
/// ownership without a database lookup on claim.
#[derive(Debug, Serialize, Deserialize)]
pub struct EnrollmentClaims {
    pub iss: String,
    pub exp: u64,
    pub iat: u64,
    pub jti: String,
    /// Owner user_id. Present on stateless claim tokens so the server
    /// can assign ownership without a DB lookup.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sub: Option<String>,
    pub server_addr: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wifi: Option<Vec<WifiCredential>>,
}

/// WiFi credentials embedded in an enrollment token.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WifiCredential {
    pub ssid: String,
    pub psk: String,
}

impl EnrollmentClaims {
    /// Create new claims with a generated UUID jti and expiry.
    /// Used by tests and legacy enrollment paths.
    #[allow(dead_code)]
    pub fn new(
        server_addr: &str,
        display_name: Option<String>,
        wifi: Option<Vec<WifiCredential>>,
    ) -> Self {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        Self {
            iss: "ghostcam-server".to_string(),
            exp: now + ghostcam::config::ENROLLMENT_TOKEN_TTL_SECS,
            iat: now,
            jti: uuid::Uuid::new_v4().to_string(),
            sub: None,
            server_addr: server_addr.to_string(),
            display_name,
            wifi,
        }
    }

    /// Create a stateless claim token with the owner's user_id embedded.
    /// No database storage needed -- the server verifies the signature and
    /// reads `sub` directly on claim.
    pub fn new_claim(server_addr: &str, user_id: &str, ttl_secs: u64) -> Self {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        Self {
            iss: "ghostcam-server".to_string(),
            exp: now + ttl_secs,
            iat: now,
            jti: uuid::Uuid::new_v4().to_string(),
            sub: Some(user_id.to_string()),
            server_addr: server_addr.to_string(),
            display_name: None,
            wifi: None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn claims_new_sets_jti() {
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        // Should be a valid UUID
        uuid::Uuid::parse_str(&claims.jti).unwrap();
    }

    #[test]
    fn claims_new_sets_exp() {
        let before = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);

        let after = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        let expected_min = before + ghostcam::config::ENROLLMENT_TOKEN_TTL_SECS;
        let expected_max = after + ghostcam::config::ENROLLMENT_TOKEN_TTL_SECS;
        assert!(claims.exp >= expected_min);
        assert!(claims.exp <= expected_max);
    }

    #[test]
    fn claims_new_sets_iss() {
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        assert_eq!(claims.iss, "ghostcam-server");
    }

    #[test]
    fn claims_with_display_name() {
        let claims = EnrollmentClaims::new("127.0.0.1:4433", Some("Kitchen".into()), None);
        assert_eq!(claims.display_name.as_deref(), Some("Kitchen"));
    }

    #[test]
    fn claims_with_wifi() {
        let wifi = vec![WifiCredential {
            ssid: "CameraNet".into(),
            psk: "pass123".into(),
        }];
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, Some(wifi));
        assert_eq!(claims.wifi.as_ref().unwrap().len(), 1);
        assert_eq!(claims.wifi.as_ref().unwrap()[0].ssid, "CameraNet");
    }

    #[test]
    fn claims_without_optionals() {
        let claims = EnrollmentClaims::new("127.0.0.1:4433", None, None);
        let json = serde_json::to_string(&claims).unwrap();
        assert!(!json.contains("display_name"));
        assert!(!json.contains("wifi"));
        assert!(!json.contains("\"sub\""));
    }

    #[test]
    fn claim_token_has_sub() {
        let claims = EnrollmentClaims::new_claim("127.0.0.1:4433", "user-123", 3600);
        assert_eq!(claims.sub.as_deref(), Some("user-123"));
        assert!(claims.iat > 0);
        assert!(claims.exp > claims.iat);
    }

    #[test]
    fn claim_token_ttl_respected() {
        let before = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        let claims = EnrollmentClaims::new_claim("127.0.0.1:4433", "user-1", 7200);

        let expected_min = before + 7200;
        assert!(claims.exp >= expected_min);
        assert!(claims.exp <= expected_min + 2); // allow 2s of test drift
    }

    #[test]
    fn claim_token_serializes_sub() {
        let claims = EnrollmentClaims::new_claim("127.0.0.1:4433", "user-abc", 3600);
        let json = serde_json::to_string(&claims).unwrap();
        assert!(json.contains("\"sub\":\"user-abc\""));
    }
}
