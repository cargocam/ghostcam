use anyhow::Result;
use async_trait::async_trait;
use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};

/// The fixed UserId used until multi-user support is implemented.
pub const SOLO_USER_ID: &str = "solo";

/// A camera record from the database.
#[derive(Debug, Clone)]
pub struct CameraRecord {
    pub device_id: DeviceId,
    pub user_id: UserId,
    pub cert_fingerprint: CertFingerprint,
    pub display_name: String,
    pub enrolled_at: u64,
    pub last_seen_at: Option<u64>,
    pub notes: Option<String>,
}

/// Fields for creating a new camera record.
#[derive(Debug, Clone)]
pub struct NewCameraRecord {
    pub user_id: UserId,
    pub cert_fingerprint: CertFingerprint,
    pub display_name: String,
}

/// Fields for updating an existing camera record.
#[derive(Debug, Clone, Default)]
pub struct CameraUpdate {
    pub display_name: Option<String>,
    pub notes: Option<String>,
}

/// Fields for creating an enrollment token.
#[derive(Debug, Clone)]
pub struct NewEnrollmentToken {
    pub jti: String,
    pub user_id: UserId,
    pub expires_at: u64,
}

/// Fields for creating a session.
#[derive(Debug, Clone)]
pub struct NewSession {
    pub session_id: SessionId,
    pub user_id: UserId,
    pub user_agent: Option<String>,
    pub ip_address: Option<String>,
}

/// A session record from the database.
#[derive(Debug, Clone)]
pub struct SessionRecord {
    pub session_id: SessionId,
    pub user_id: UserId,
    pub created_at: u64,
    pub expires_at: u64,
    pub last_active_at: Option<u64>,
}

/// Fields for creating an API token.
#[derive(Debug, Clone)]
pub struct NewApiToken {
    pub token_id: TokenId,
    pub user_id: UserId,
    pub token_hash: String,
    pub label: String,
    pub expires_at: Option<u64>,
}

/// An API token record from the database.
#[derive(Debug, Clone)]
pub struct ApiTokenRecord {
    pub token_id: TokenId,
    pub user_id: UserId,
    pub label: String,
    pub created_at: u64,
    pub expires_at: Option<u64>,
    pub last_used_at: Option<u64>,
}

/// A user record from the database.
#[derive(Debug, Clone)]
pub struct UserRecord {
    pub user_id: UserId,
    pub email: String,
    pub display_name: String,
    pub created_at: u64,
    pub verified_at: Option<u64>,
    pub disabled_at: Option<u64>,
}

/// Fields for updating a user record.
#[derive(Debug, Clone, Default)]
pub struct UserUpdate {
    pub email: Option<String>,
    pub display_name: Option<String>,
    pub password_hash: Option<String>,
}

/// Async database trait. PostgreSQL implementation in db.rs.
#[async_trait]
pub trait Database: Send + Sync + 'static {
    // --- Camera operations ---
    async fn get_camera_by_fingerprint(
        &self,
        fingerprint: &CertFingerprint,
    ) -> Result<Option<CameraRecord>>;
    async fn get_camera(&self, device_id: &DeviceId) -> Result<Option<CameraRecord>>;
    async fn list_cameras(&self, user_id: &UserId) -> Result<Vec<CameraRecord>>;
    async fn create_camera(&self, record: &NewCameraRecord) -> Result<CameraRecord>;
    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()>;
    async fn delete_camera(&self, device_id: &DeviceId) -> Result<()>;
    async fn update_last_seen(&self, device_id: &DeviceId) -> Result<()>;

    // --- Enrollment tokens ---
    async fn create_enrollment_token(&self, token: &NewEnrollmentToken) -> Result<()>;
    async fn claim_enrollment_token(&self, jti: &str, device_id: &DeviceId) -> Result<bool>;
    async fn cleanup_expired_tokens(&self) -> Result<u64>;

    // --- Sessions ---
    async fn create_session(&self, session: &NewSession) -> Result<()>;
    async fn get_session(&self, session_id: &SessionId) -> Result<Option<SessionRecord>>;
    async fn delete_session(&self, session_id: &SessionId) -> Result<()>;
    async fn extend_session(&self, session_id: &SessionId) -> Result<()>;
    async fn cleanup_expired_sessions(&self) -> Result<u64>;

    // --- API tokens ---
    async fn create_api_token(&self, token: &NewApiToken) -> Result<()>;
    async fn list_api_tokens(&self, user_id: &UserId) -> Result<Vec<ApiTokenRecord>>;
    async fn verify_api_token(&self, token_hash: &str) -> Result<Option<ApiTokenRecord>>;
    async fn delete_api_token(&self, token_id: &TokenId) -> Result<()>;

    // --- Auth ---
    async fn verify_password(&self, user_id: &UserId, password_hash: &str) -> Result<bool>;
    async fn set_password(&self, user_id: &UserId, password_hash: &str) -> Result<()>;

    // --- Server config ---
    async fn get_hmac_secret(&self) -> Result<Vec<u8>>;

    // --- Lifecycle ---
    /// Run database migrations. Called once at startup.
    async fn migrate(&self) -> Result<()>;

    /// Check if the database is reachable (for health checks).
    async fn health_check(&self) -> Result<()>;

    // --- User management ---
    async fn create_user(
        &self,
        email: &str,
        password_hash: &str,
        display_name: &str,
    ) -> Result<UserId>;
    async fn get_user_by_email(&self, email: &str) -> Result<Option<UserRecord>>;
    async fn get_user(&self, user_id: &UserId) -> Result<Option<UserRecord>>;
    async fn update_user(&self, user_id: &UserId, update: &UserUpdate) -> Result<()>;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trait_is_object_safe() {
        // This test verifies the Database trait can be used as a trait object.
        fn _assert_object_safe(_: &dyn Database) {}
    }
}
