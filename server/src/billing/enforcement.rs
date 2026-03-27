use anyhow::Result;
use ghostcam::types::UserId;

use crate::billing::tiers::TierRegistry;
use crate::db_trait::Database;

#[derive(Debug)]
pub enum EnforcementError {
    CameraLimitReached { current: i64, limit: u32 },
    SubscriptionSuspended,
}

impl std::fmt::Display for EnforcementError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::CameraLimitReached { current, limit } => {
                write!(
                    f,
                    "Camera limit reached: {current} of {limit} cameras used"
                )
            }
            Self::SubscriptionSuspended => {
                write!(f, "Subscription is suspended due to payment failure")
            }
        }
    }
}

/// Check whether a user can enroll another camera.
///
/// When `billing_enabled` is false (no Stripe configured), always allows enrollment.
pub async fn check_camera_limit(
    db: &dyn Database,
    user_id: &UserId,
    tiers: &TierRegistry,
    billing_enabled: bool,
) -> Result<Result<(), EnforcementError>> {
    if !billing_enabled {
        return Ok(Ok(()));
    }

    let sub = db.get_subscription(user_id).await?;
    let tier_id = sub.as_ref().map(|s| s.tier.as_str()).unwrap_or("free");
    let status = sub
        .as_ref()
        .map(|s| s.status.as_str())
        .unwrap_or("active");

    // Suspended users cannot enroll new cameras
    if status == "suspended" {
        return Ok(Err(EnforcementError::SubscriptionSuspended));
    }

    // Check camera limit
    if let Some(max) = tiers.camera_limit(tier_id) {
        let current = db.get_camera_count(user_id).await?;
        if current >= max as i64 {
            return Ok(Err(EnforcementError::CameraLimitReached {
                current,
                limit: max,
            }));
        }
    }

    Ok(Ok(()))
}
