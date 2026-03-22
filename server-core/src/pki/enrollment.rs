use anyhow::Result;
use ghostcam::types::UserId;
use serde::{Deserialize, Serialize};

use super::ca::CaManager;
use crate::db::{Database, NewEnrollmentToken};

/// JWT claims for an enrollment token.
#[derive(Debug, Serialize, Deserialize)]
pub struct EnrollmentClaims {
    pub iss: String,
    pub exp: u64,
    pub jti: String,
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
            jti: uuid::Uuid::new_v4().to_string(),
            server_addr: server_addr.to_string(),
            display_name,
            wifi,
        }
    }
}

/// Generate a signed enrollment JWT and record the token in the database.
pub async fn create_enrollment_token(
    ca: &CaManager,
    db: &dyn Database,
    user_id: &UserId,
    server_addr: &str,
    display_name: Option<String>,
    wifi: Option<Vec<WifiCredential>>,
) -> Result<String> {
    let claims = EnrollmentClaims::new(server_addr, display_name, wifi);

    db.create_enrollment_token(&NewEnrollmentToken {
        jti: claims.jti.clone(),
        user_id: user_id.clone(),
        expires_at: claims.exp,
    })
    .await?;

    let token = ca.sign_enrollment_jwt(&claims)?;
    Ok(token)
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
    }
}
