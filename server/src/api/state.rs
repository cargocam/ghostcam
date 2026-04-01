use std::sync::Arc;

use ghostcam::firmware::FirmwareRelease;
use tokio::sync::RwLock;

use crate::audit::AuditLogger;
use crate::billing::stripe_client::StripeClient;
use crate::billing::tiers::TierRegistry;
use crate::db_trait::Database;
use crate::redis::connection::RedisManager;
use crate::s3::S3Client;

/// Shared application state passed to all Axum handlers.
pub struct AppState {
    pub db: Arc<dyn Database>,
    pub redis: Option<Arc<RedisManager>>,
    pub hmac_secret: Vec<u8>,
    pub audit: Arc<AuditLogger>,
    /// S3/Tigris client for presigned URL generation. None in tests or if unconfigured.
    pub s3: Option<Arc<S3Client>>,
    /// Stripe client for billing. None = billing disabled (unlimited free tier).
    pub stripe: Option<Arc<StripeClient>>,
    /// Subscription tier definitions.
    pub tiers: Arc<TierRegistry>,
    /// Stripe publishable key (needed by frontend for Pricing Table embed).
    pub stripe_public_key: Option<String>,
    /// Stripe Pricing Table ID (free-tier users see this to pick a plan).
    pub stripe_pricing_table_id: Option<String>,
    /// Stripe portal configuration ID (enables plan switching in portal).
    pub stripe_portal_config_id: Option<String>,
    /// Latest known firmware release (populated by GitHub webhook or startup fetch).
    pub firmware_release: Arc<RwLock<Option<FirmwareRelease>>>,
    /// GitHub webhook secret for signature verification. None = webhook disabled.
    pub github_webhook_secret: Option<String>,
    /// Window (seconds) over which to spread reboot commands on new release.
    pub update_stagger_secs: u64,
    /// Version for which a staggered reboot is already in flight.
    pub pending_reboot_version: tokio::sync::Mutex<Option<String>>,
}
