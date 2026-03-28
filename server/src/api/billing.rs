use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Extension;
use axum::Json;
use serde::{Deserialize, Serialize};

use super::auth::AuthUser;
use super::state::AppState;
use crate::billing::stripe_client::WebhookAction;
use crate::db_trait::SubscriptionRecord;

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

fn current_month() -> String {
    let now = chrono::Utc::now();
    now.format("%Y-%m").to_string()
}

// --- Response types ---

#[derive(Serialize)]
pub struct SubscriptionResponse {
    pub tier: String,
    pub status: String,
    pub billing_enabled: bool,
    pub current_period_end: Option<u64>,
    pub grace_expires_at: Option<u64>,
    /// Stripe publishable key for Pricing Table embed.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stripe_public_key: Option<String>,
    /// Stripe Pricing Table ID for upgrade flow.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stripe_pricing_table_id: Option<String>,
}

#[derive(Serialize)]
pub struct UsageResponse {
    pub cameras_count: i32,
    pub storage_bytes: i64,
    pub bandwidth_bytes: i64,
    pub camera_limit: Option<u32>,
    pub storage_limit_gb: Option<u64>,
    pub bandwidth_limit_gb: Option<u64>,
}

#[derive(Deserialize)]
pub struct PortalRequest {
    pub return_url: String,
}

#[derive(Serialize)]
pub struct PortalResponse {
    pub url: String,
}

/// GET /api/v1/billing/subscription
pub async fn get_subscription(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Response {
    let billing_enabled = state.stripe.is_some();

    if !billing_enabled {
        return Json(SubscriptionResponse {
            tier: "unlimited".into(),
            status: "active".into(),
            billing_enabled: false,
            current_period_end: None,
            grace_expires_at: None,
            stripe_public_key: None,
            stripe_pricing_table_id: None,
        })
        .into_response();
    }

    let pk = state.stripe_public_key.clone();
    let pt = state.stripe_pricing_table_id.clone();

    match state.db.get_subscription(&user.user_id).await {
        Ok(Some(sub)) => Json(SubscriptionResponse {
            tier: sub.tier,
            status: sub.status,
            billing_enabled: true,
            current_period_end: sub.current_period_end,
            grace_expires_at: sub.grace_expires_at,
            stripe_public_key: pk,
            stripe_pricing_table_id: pt,
        })
        .into_response(),
        Ok(None) => Json(SubscriptionResponse {
            tier: "free".into(),
            status: "active".into(),
            billing_enabled: true,
            current_period_end: None,
            grace_expires_at: None,
            stripe_public_key: pk,
            stripe_pricing_table_id: pt,
        })
        .into_response(),
        Err(e) => {
            tracing::error!("get subscription error: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

/// GET /api/v1/billing/tiers
pub async fn list_tiers(State(state): State<Arc<AppState>>) -> Response {
    if state.stripe.is_none() {
        return Json(serde_json::json!([])).into_response();
    }
    Json(state.tiers.all()).into_response()
}

/// POST /api/v1/billing/portal
pub async fn create_portal(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
    Json(body): Json<PortalRequest>,
) -> Response {
    let stripe = match &state.stripe {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    if !body.return_url.starts_with("http://") && !body.return_url.starts_with("https://") {
        return (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error": "invalid return_url"})),
        )
            .into_response();
    }

    // Get or create Stripe customer for this user
    let sub = state.db.get_subscription(&user.user_id).await;
    let customer_id = match sub {
        Ok(Some(ref s)) if s.stripe_customer_id.is_some() => {
            s.stripe_customer_id.clone().unwrap()
        }
        _ => {
            // Create a Stripe customer so the portal can be opened
            let user_record = match state.db.get_user(&user.user_id).await {
                Ok(Some(u)) => u,
                _ => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
            };
            match stripe
                .create_customer(&user_record.email, &user.user_id.0)
                .await
            {
                Ok(cid) => {
                    let now = now_unix();
                    let record = SubscriptionRecord {
                        user_id: user.user_id.clone(),
                        stripe_customer_id: Some(cid.clone()),
                        stripe_subscription_id: None,
                        tier: "free".into(),
                        status: "active".into(),
                        current_period_start: None,
                        current_period_end: None,
                        grace_expires_at: None,
                        created_at: sub
                            .as_ref()
                            .ok()
                            .and_then(|s| s.as_ref())
                            .map(|s| s.created_at)
                            .unwrap_or(now),
                        updated_at: now,
                    };
                    if let Err(e) = state.db.upsert_subscription(&record).await {
                        tracing::error!("failed to store Stripe customer: {e}");
                        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                    }
                    cid
                }
                Err(e) => {
                    tracing::error!("failed to create Stripe customer: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }
            }
        }
    };

    match stripe
        .create_portal_session(
            &customer_id,
            &body.return_url,
            state.stripe_portal_config_id.as_deref(),
        )
        .await
    {
        Ok(url) => Json(PortalResponse { url }).into_response(),
        Err(e) => {
            tracing::error!("failed to create portal session: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

/// GET /api/v1/billing/usage
pub async fn get_usage(
    State(state): State<Arc<AppState>>,
    Extension(user): Extension<AuthUser>,
) -> Response {
    let month = current_month();

    // Camera count from the live cameras table
    let cameras_count = match state.db.get_camera_count(&user.user_id).await {
        Ok(c) => c as i32,
        Err(e) => {
            tracing::error!("get camera count error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Bandwidth/storage from Redis counters
    let (bandwidth_bytes, storage_bytes) = if let Some(ref redis) = state.redis {
        crate::redis::usage::get_usage(redis, &user.user_id.0, &month).await
    } else {
        (0, 0)
    };

    // Look up tier limits
    let sub = state
        .db
        .get_subscription(&user.user_id)
        .await
        .ok()
        .flatten();
    let tier_id = sub.as_ref().map(|s| s.tier.as_str()).unwrap_or("free");
    let tier = state.tiers.get(tier_id);

    Json(UsageResponse {
        cameras_count,
        storage_bytes,
        bandwidth_bytes,
        camera_limit: tier.and_then(|t| t.camera_limit),
        storage_limit_gb: tier.and_then(|t| t.storage_gb),
        bandwidth_limit_gb: tier.and_then(|t| t.bandwidth_gb),
    })
    .into_response()
}

/// POST /api/v1/webhooks/stripe
///
/// Public endpoint (no auth middleware). Verified by Stripe webhook signature.
pub async fn stripe_webhook(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let stripe = match &state.stripe {
        Some(s) => s,
        None => return StatusCode::NOT_FOUND.into_response(),
    };

    let signature = match headers
        .get("stripe-signature")
        .and_then(|v| v.to_str().ok())
    {
        Some(s) => s,
        None => return StatusCode::BAD_REQUEST.into_response(),
    };

    let payload = match std::str::from_utf8(&body) {
        Ok(s) => s,
        Err(_) => return StatusCode::BAD_REQUEST.into_response(),
    };

    let event = match stripe.verify_webhook(payload, signature) {
        Ok(e) => e,
        Err(e) => {
            tracing::warn!("webhook signature verification failed: {e}");
            return StatusCode::BAD_REQUEST.into_response();
        }
    };

    let event_id = event.id.to_string();

    // Atomic idempotency: attempt INSERT, skip if already claimed
    match state.db.try_claim_stripe_event(&event_id).await {
        Ok(false) => return StatusCode::OK.into_response(),
        Err(e) => {
            tracing::error!("idempotency check error: {e}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
        Ok(true) => {}
    }

    let action = stripe.parse_event(&event);
    let now = now_unix();

    match action {
        WebhookAction::CheckoutCompleted {
            customer_id,
            subscription_id,
            client_reference_id,
        } => {
            // Find user by client_reference_id or by stripe customer
            let user_id = if let Some(ref uid) = client_reference_id {
                Some(ghostcam::types::UserId(uid.clone()))
            } else {
                state
                    .db
                    .get_subscription_by_stripe_customer(&customer_id)
                    .await
                    .ok()
                    .flatten()
                    .map(|s| s.user_id)
            };

            if let Some(user_id) = user_id {
                let existing = state.db.get_subscription(&user_id).await.ok().flatten();
                let existing_tier = existing
                    .as_ref()
                    .map(|s| s.tier.clone())
                    .unwrap_or_else(|| "free".into());
                let created_at = existing.as_ref().map(|s| s.created_at).unwrap_or(now);
                let record = SubscriptionRecord {
                    user_id: user_id.clone(),
                    stripe_customer_id: Some(customer_id),
                    stripe_subscription_id: Some(subscription_id),
                    tier: existing_tier,
                    status: "active".into(),
                    current_period_start: None,
                    current_period_end: None,
                    grace_expires_at: None,
                    created_at,
                    updated_at: now,
                };
                if let Err(e) = state.db.upsert_subscription(&record).await {
                    tracing::error!("checkout webhook upsert error: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }
            }
        }
        WebhookAction::SubscriptionUpdated {
            customer_id,
            subscription_id,
            status,
            current_period_start,
            current_period_end,
            tier,
        } => {
            if let Ok(Some(existing)) = state
                .db
                .get_subscription_by_stripe_customer(&customer_id)
                .await
            {
                let new_tier = tier.unwrap_or_else(|| existing.tier.clone());

                let old_tier = existing.tier.clone();
                let record = SubscriptionRecord {
                    user_id: existing.user_id.clone(),
                    stripe_customer_id: Some(customer_id),
                    stripe_subscription_id: Some(subscription_id),
                    tier: new_tier.clone(),
                    status: status.clone(),
                    current_period_start,
                    current_period_end,
                    grace_expires_at: existing.grace_expires_at,
                    created_at: existing.created_at,
                    updated_at: now,
                };
                if let Err(e) = state.db.upsert_subscription(&record).await {
                    tracing::error!("subscription update webhook error: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }

                if old_tier != new_tier {
                    state
                        .audit
                        .log(crate::audit::AuditEvent::SubscriptionChanged {
                            user_id: existing.user_id.0.clone(),
                            old_tier,
                            new_tier,
                            status,
                        });
                }
            }
        }
        WebhookAction::SubscriptionDeleted { customer_id } => {
            if let Ok(Some(existing)) = state
                .db
                .get_subscription_by_stripe_customer(&customer_id)
                .await
            {
                let old_tier = existing.tier.clone();
                let record = SubscriptionRecord {
                    tier: "free".into(),
                    status: "canceled".into(),
                    stripe_subscription_id: None,
                    grace_expires_at: None,
                    updated_at: now,
                    ..existing.clone()
                };
                if let Err(e) = state.db.upsert_subscription(&record).await {
                    tracing::error!("subscription delete webhook error: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }

                state
                    .audit
                    .log(crate::audit::AuditEvent::SubscriptionChanged {
                        user_id: existing.user_id.0.clone(),
                        old_tier,
                        new_tier: "free".into(),
                        status: "canceled".into(),
                    });
            }
        }
        WebhookAction::InvoicePaymentSucceeded { customer_id } => {
            if let Ok(Some(existing)) = state
                .db
                .get_subscription_by_stripe_customer(&customer_id)
                .await
            {
                let record = SubscriptionRecord {
                    status: "active".into(),
                    grace_expires_at: None,
                    updated_at: now,
                    ..existing
                };
                if let Err(e) = state.db.upsert_subscription(&record).await {
                    tracing::error!("payment succeeded webhook error: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }
            }
        }
        WebhookAction::InvoicePaymentFailed { customer_id } => {
            if let Ok(Some(existing)) = state
                .db
                .get_subscription_by_stripe_customer(&customer_id)
                .await
            {
                let grace_expires_at = existing.grace_expires_at.unwrap_or(now + 7 * 86400);
                let record = SubscriptionRecord {
                    status: "past_due".into(),
                    grace_expires_at: Some(grace_expires_at),
                    updated_at: now,
                    ..existing
                };
                if let Err(e) = state.db.upsert_subscription(&record).await {
                    tracing::error!("payment failed webhook error: {e}");
                    return StatusCode::INTERNAL_SERVER_ERROR.into_response();
                }
            }
        }
        WebhookAction::Unknown => {
            tracing::debug!(event_type = ?event.type_, "unhandled webhook event type");
        }
    }

    StatusCode::OK.into_response()
}
