use anyhow::Result;
use async_trait::async_trait;
use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};

/// A camera record from the database.
#[derive(Debug, Clone)]
pub struct CameraRecord {
    pub device_id: DeviceId,
    /// Owner user ID. None if the device is unclaimed.
    pub user_id: Option<UserId>,
    pub cert_fingerprint: CertFingerprint,
    pub display_name: String,
    pub enrolled_at: u64,
    pub last_seen_at: Option<u64>,
    pub notes: Option<String>,
}

/// Fields for updating an existing camera record.
#[derive(Debug, Clone, Default)]
pub struct CameraUpdate {
    pub display_name: Option<String>,
    pub notes: Option<String>,
}

/// Fields for creating an enrollment token (legacy DB-backed flow).
#[allow(dead_code)]
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
#[allow(dead_code)]
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
#[allow(dead_code)]
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
#[allow(dead_code)]
#[derive(Debug, Clone, Default)]
pub struct UserUpdate {
    pub email: Option<String>,
    pub display_name: Option<String>,
    pub password_hash: Option<String>,
}

/// An audit log record from the database.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct AuditLogRecord {
    pub id: i64,
    pub timestamp: String,
    pub event_type: String,
    pub event_data: serde_json::Value,
    pub hmac: String,
}

/// A segment metadata record.
#[derive(Debug, Clone)]
pub struct SegmentRecord {
    pub segment_id: String,
    pub device_id: DeviceId,
    pub s3_key: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
    pub resolution: String,
    pub created_at: u64,
}

/// A provision token record.
#[derive(Debug, Clone)]
pub struct ProvisionTokenRecord {
    pub token_hash: String,
    pub user_id: UserId,
    pub created_at: u64,
    pub expires_at: u64,
    pub claimed_at: Option<u64>,
    pub device_id: Option<DeviceId>,
}

/// A subscription record from the database.
#[derive(Debug, Clone)]
pub struct SubscriptionRecord {
    pub user_id: UserId,
    pub stripe_customer_id: Option<String>,
    pub stripe_subscription_id: Option<String>,
    pub tier: String,
    pub status: String,
    pub current_period_start: Option<u64>,
    pub current_period_end: Option<u64>,
    pub grace_expires_at: Option<u64>,
    pub created_at: u64,
    pub updated_at: u64,
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
    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()>;
    async fn delete_camera(&self, device_id: &DeviceId) -> Result<()>;
    async fn update_last_seen(&self, device_id: &DeviceId) -> Result<()>;

    /// Register a device (auto-registration on first QUIC connect). No owner.
    async fn register_device(
        &self,
        fingerprint: &CertFingerprint,
        display_name: &str,
    ) -> Result<CameraRecord>;

    /// Claim a device — assign it to a user.
    async fn claim_device(&self, device_id: &DeviceId, user_id: &UserId) -> Result<()>;

    /// Get unclaimed devices (user_id IS NULL).
    async fn get_unclaimed_devices(&self) -> Result<Vec<CameraRecord>>;

    /// Get the owner of a device. Returns None if unclaimed.
    #[allow(dead_code)]
    async fn get_device_owner(&self, device_id: &DeviceId) -> Result<Option<UserId>>;

    // --- Enrollment tokens (legacy DB-backed flow, kept for backward compat) ---
    #[allow(dead_code)]
    async fn create_enrollment_token(&self, token: &NewEnrollmentToken) -> Result<()>;
    async fn claim_enrollment_token(&self, jti: &str, device_id: &DeviceId) -> Result<bool>;
    #[allow(dead_code)]
    async fn cleanup_expired_tokens(&self) -> Result<u64>;
    async fn get_enrollment_token_user_id(&self, jti: &str) -> Result<Option<UserId>>;

    // --- Sessions ---
    async fn create_session(&self, session: &NewSession) -> Result<()>;
    async fn get_session(&self, session_id: &SessionId) -> Result<Option<SessionRecord>>;
    async fn delete_session(&self, session_id: &SessionId) -> Result<()>;
    async fn extend_session(&self, session_id: &SessionId) -> Result<()>;
    #[allow(dead_code)]
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
    #[allow(dead_code)]
    async fn get_user(&self, user_id: &UserId) -> Result<Option<UserRecord>>;
    #[allow(dead_code)]
    async fn update_user(&self, user_id: &UserId, update: &UserUpdate) -> Result<()>;

    // --- Audit log ---
    async fn insert_audit_entry(
        &self,
        timestamp: &str,
        event_type: &str,
        event_data: &serde_json::Value,
        hmac: &str,
    ) -> Result<()>;

    // --- Billing ---
    async fn get_subscription(&self, user_id: &UserId) -> Result<Option<SubscriptionRecord>>;
    async fn get_subscription_by_stripe_customer(
        &self,
        customer_id: &str,
    ) -> Result<Option<SubscriptionRecord>>;
    async fn upsert_subscription(&self, record: &SubscriptionRecord) -> Result<()>;
    async fn get_camera_count(&self, user_id: &UserId) -> Result<i64>;
    async fn list_past_due_expired(&self, now: u64) -> Result<Vec<SubscriptionRecord>>;

    // --- Segments (HLS/S3) ---
    async fn insert_segments(&self, segments: &[SegmentRecord]) -> Result<()>;
    async fn list_segments(
        &self,
        device_id: &DeviceId,
        from_ts: u64,
        to_ts: u64,
    ) -> Result<Vec<SegmentRecord>>;
    /// Get the latest segment for a device (for live manifest generation).
    async fn latest_segment(&self, device_id: &DeviceId) -> Result<Option<SegmentRecord>>;

    // --- Provisioned camera creation ---
    /// Create a camera via provisioning (with user_id and device_serial set).
    async fn create_provisioned_camera(
        &self,
        device_id: &DeviceId,
        user_id: &UserId,
        device_serial: &str,
    ) -> Result<()>;

    // --- Camera API keys ---
    /// Look up a camera by its API key hash. Returns (device_id, user_id).
    async fn get_camera_by_api_key(&self, api_key_hash: &str) -> Result<Option<CameraRecord>>;
    async fn create_camera_api_key(&self, device_id: &DeviceId, api_key_hash: &str) -> Result<()>;

    // --- Provision tokens ---
    async fn create_provision_token(
        &self,
        token_hash: &str,
        user_id: &UserId,
        expires_at: u64,
    ) -> Result<()>;
    /// Claim a provision token. Returns the user_id if valid and unclaimed.
    async fn claim_provision_token(
        &self,
        token_hash: &str,
        device_id: &DeviceId,
    ) -> Result<Option<UserId>>;

    // --- Camera commands ---
    /// Queue a command for a camera to pick up on next telemetry poll.
    async fn enqueue_command(
        &self,
        device_id: &DeviceId,
        command: &serde_json::Value,
    ) -> Result<()>;
    /// Claim and return all pending commands for a camera.
    async fn claim_commands(&self, device_id: &DeviceId) -> Result<Vec<serde_json::Value>>;

    // --- Stripe idempotency ---
    async fn try_claim_stripe_event(&self, event_id: &str) -> Result<bool>;

    async fn cleanup_old_stripe_events(&self, before: u64) -> Result<u64>;

    /// Returns `(entries, total)` in a single query using `COUNT(*) OVER()`.
    async fn query_audit_log(
        &self,
        event_type: Option<&str>,
        since: Option<&str>,
        until: Option<&str>,
        limit: i64,
        offset: i64,
    ) -> Result<(Vec<AuditLogRecord>, i64)>;
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
