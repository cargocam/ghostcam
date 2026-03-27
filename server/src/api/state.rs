use std::sync::Arc;

use crate::audit::AuditLogger;
use crate::billing::stripe_client::StripeClient;
use crate::billing::tiers::TierRegistry;
use crate::db_trait::Database;
use crate::egress::sessions::SessionManager;
use crate::egress::udp::SharedWebRtcSocket;
use crate::ingest::registry::RoutingRegistry;
use crate::pki::ca::CaManager;
use crate::pki::revocation::RevocationCache;
use crate::redis::connection::RedisManager;
use crate::sse::SseEventBus;

/// Shared application state passed to all Axum handlers.
pub struct AppState {
    pub db: Arc<dyn Database>,
    pub redis: Option<Arc<RedisManager>>,
    pub registry: Arc<RoutingRegistry>,
    pub sessions: Arc<SessionManager>,
    pub sse_bus: Arc<SseEventBus>,
    pub ca: Arc<CaManager>,
    pub revocation_cache: Arc<RevocationCache>,
    pub hmac_secret: Vec<u8>,
    pub audit: Arc<AuditLogger>,
    /// Explicit public IP override (from `GHOSTCAM_PUBLIC_IP`). When set, all
    /// ICE candidates use this IP. When `None`, the server derives the ICE
    /// candidate IP from the HTTP request's `Host` header so that the browser
    /// can reach the WebRTC UDP port via the same hostname it used for HTTP.
    pub public_ip_override: Option<std::net::IpAddr>,
    /// Address embedded in enrollment JWTs. Defaults to public_addr but can be
    /// overridden via GHOSTCAM_ENROLLMENT_ADDR for split-horizon deployments
    /// (e.g. Docker: cameras use service DNS, browsers use LAN IP).
    pub enrollment_addr: String,
    /// Shared UDP socket for all WebRTC sessions (demultiplexed by STUN ufrag).
    pub webrtc_socket: Arc<SharedWebRtcSocket>,
    /// Stripe client for billing. None = billing disabled (unlimited free tier).
    pub stripe: Option<Arc<StripeClient>>,
    /// Subscription tier definitions.
    pub tiers: Arc<TierRegistry>,
}
