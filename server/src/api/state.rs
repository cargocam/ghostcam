use std::net::SocketAddr;
use std::sync::Arc;

use crate::db_trait::Database;
use crate::egress::sessions::SessionManager;
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
    pub public_addr: SocketAddr,
}
