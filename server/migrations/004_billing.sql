-- 004_billing.sql: Subscriptions and Stripe webhook idempotency
-- Usage tracking (bandwidth, storage) lives in Redis, not PostgreSQL.

-- Per-user subscription state (one row per user, upserted by webhooks)
CREATE TABLE IF NOT EXISTS subscriptions (
    user_id                 TEXT PRIMARY KEY REFERENCES users(user_id),
    stripe_customer_id      TEXT UNIQUE,
    stripe_subscription_id  TEXT UNIQUE,
    tier                    TEXT NOT NULL DEFAULT 'free',
    status                  TEXT NOT NULL DEFAULT 'active',
    current_period_start    BIGINT,
    current_period_end      BIGINT,
    grace_expires_at        BIGINT,
    created_at              BIGINT NOT NULL,
    updated_at              BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_subscriptions_grace ON subscriptions(grace_expires_at)
    WHERE grace_expires_at IS NOT NULL;

-- Stripe webhook idempotency (store processed event IDs)
CREATE TABLE IF NOT EXISTS stripe_events (
    event_id        TEXT PRIMARY KEY,
    processed_at    BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_stripe_events_processed ON stripe_events(processed_at);

-- Drop usage_records if it exists from a previous migration run
DROP TABLE IF EXISTS usage_records;
