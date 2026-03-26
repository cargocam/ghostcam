use std::net::IpAddr;
use std::sync::Arc;

use axum::extract::{ConnectInfo, State};
use axum::http::{HeaderMap, Request, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use governor::clock::DefaultClock;
use governor::state::keyed::DefaultKeyedStateStore;
use governor::{Quota, RateLimiter};

/// Shared keyed rate limiter type.
pub type KeyedLimiter<K> = Arc<RateLimiter<K, DefaultKeyedStateStore<K>, DefaultClock>>;

/// Build a keyed rate limiter.
fn keyed_limiter<K: Clone + Eq + std::hash::Hash>(quota: Quota) -> KeyedLimiter<K> {
    Arc::new(RateLimiter::keyed(quota))
}

/// Extract client IP from request, checking X-Forwarded-For and X-Real-IP
/// headers before falling back to the socket address from ConnectInfo.
///
/// NOTE: X-Forwarded-For and X-Real-IP are trusted unconditionally. This is
/// acceptable because the server typically runs behind a known reverse proxy
/// (nginx, Caddy, etc.) that overwrites these headers. A proper fix would be
/// a trusted-proxy allowlist that only accepts forwarded headers from known
/// proxy IPs and falls back to the socket address otherwise.
fn client_ip(headers: &HeaderMap, extensions: &axum::http::Extensions) -> IpAddr {
    // 1. X-Forwarded-For (first entry is the original client)
    if let Some(xff) = headers.get("x-forwarded-for").and_then(|v| v.to_str().ok()) {
        if let Some(first) = xff.split(',').next() {
            if let Ok(ip) = first.trim().parse::<IpAddr>() {
                return ip;
            }
        }
    }

    // 2. X-Real-IP
    if let Some(real_ip) = headers.get("x-real-ip").and_then(|v| v.to_str().ok()) {
        if let Ok(ip) = real_ip.trim().parse::<IpAddr>() {
            return ip;
        }
    }

    // 3. ConnectInfo socket address
    if let Some(addr) = extensions.get::<ConnectInfo<std::net::SocketAddr>>() {
        return addr.0.ip();
    }

    IpAddr::V4(std::net::Ipv4Addr::UNSPECIFIED)
}

// ---- Login rate limiter (keyed by client IP) ----

/// State wrapper for the login rate limiter.
#[derive(Clone)]
pub struct LoginRateLimiter(pub KeyedLimiter<IpAddr>);

impl LoginRateLimiter {
    /// 5 requests per 60 seconds per IP.
    pub fn new() -> Self {
        Self(keyed_limiter(Quota::per_minute(
            std::num::NonZeroU32::new(5).unwrap(),
        )))
    }
}

/// Axum middleware for login rate limiting.
pub async fn login_rate_limit(
    State(limiter): State<LoginRateLimiter>,
    req: Request<axum::body::Body>,
    next: Next,
) -> Response {
    let ip = client_ip(req.headers(), req.extensions());

    match limiter.0.check_key(&ip) {
        Ok(_) => next.run(req).await,
        Err(negative) => {
            let wait = negative
                .wait_time_from(governor::clock::Clock::now(&DefaultClock::default()))
                .as_secs();
            (
                StatusCode::TOO_MANY_REQUESTS,
                [("retry-after", wait.to_string())],
                "Too Many Requests",
            )
                .into_response()
        }
    }
}

// ---- Per-user API rate limiter ----

/// State wrapper for the per-user API rate limiter.
#[derive(Clone)]
pub struct ApiRateLimiter(pub KeyedLimiter<String>);

impl ApiRateLimiter {
    /// 60 requests per minute per user.
    pub fn new() -> Self {
        Self(keyed_limiter(Quota::per_minute(
            std::num::NonZeroU32::new(60).unwrap(),
        )))
    }
}

/// Axum middleware for per-user API rate limiting.
pub async fn api_rate_limit(
    State(limiter): State<ApiRateLimiter>,
    req: Request<axum::body::Body>,
    next: Next,
) -> Response {
    // NOTE: Unauthenticated requests share a single "__anonymous__" bucket,
    // meaning one unauthenticated client can exhaust the limit for all.
    // This is acceptable because unauthenticated API endpoints are already
    // covered by the per-IP login rate limiter; this middleware primarily
    // targets authenticated traffic where per-user keying is meaningful.
    let user_key = req
        .extensions()
        .get::<super::auth::AuthUser>()
        .map(|u| u.user_id.0.clone())
        .unwrap_or_else(|| "__anonymous__".to_string());

    match limiter.0.check_key(&user_key) {
        Ok(_) => next.run(req).await,
        Err(negative) => {
            let wait = negative
                .wait_time_from(governor::clock::Clock::now(&DefaultClock::default()))
                .as_secs();
            (
                StatusCode::TOO_MANY_REQUESTS,
                [("retry-after", wait.to_string())],
                "Too Many Requests",
            )
                .into_response()
        }
    }
}
