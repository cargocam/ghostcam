use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio_util::sync::CancellationToken;

use crate::audit::{AuditEvent, AuditLogger};
use crate::db_trait::Database;

/// Spawn the hourly grace period checker.
///
/// Transitions `past_due` subscriptions to `suspended` once their grace period
/// has expired. Also cleans up old Stripe event records (>90 days).
pub fn spawn_grace_period_check(
    db: Arc<dyn Database>,
    audit: Arc<AuditLogger>,
    cancel: CancellationToken,
) {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(3600));
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                _ = interval.tick() => {
                    let now = SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap()
                        .as_secs();

                    // Suspend expired grace periods
                    match db.list_past_due_expired(now).await {
                        Ok(subs) => {
                            for sub in subs {
                                let mut updated = sub.clone();
                                updated.status = "suspended".into();
                                updated.updated_at = now;
                                if let Err(e) = db.upsert_subscription(&updated).await {
                                    tracing::error!(
                                        user_id = %sub.user_id.0,
                                        "failed to suspend subscription: {e}"
                                    );
                                    continue;
                                }
                                audit.log(AuditEvent::SubscriptionSuspended {
                                    user_id: sub.user_id.0.clone(),
                                });
                                tracing::info!(
                                    user_id = %sub.user_id.0,
                                    "subscription suspended after grace period"
                                );
                            }
                        }
                        Err(e) => tracing::error!("grace period check failed: {e}"),
                    }

                    // Cleanup old stripe events (>90 days)
                    let cutoff = now.saturating_sub(90 * 86400);
                    if let Err(e) = db.cleanup_old_stripe_events(cutoff).await {
                        tracing::error!("stripe event cleanup failed: {e}");
                    }
                }
            }
        }
    });
}
