-- Migration 015: support tickets.
--
-- Inbound support email audit trail. One row per received
-- Resend inbound webhook (keyed by svix-id for idempotent dedupe).
-- Populated first with the raw email (status='received'), then with
-- classification + Linear issue URL once the async triage pipeline
-- completes (status='routed'), or with an error message if routing
-- fails (status='failed').

CREATE TABLE IF NOT EXISTS support_tickets (
    id                TEXT PRIMARY KEY,    -- svix-id (idempotency key)
    from_email        TEXT NOT NULL,
    subject           TEXT NOT NULL,
    body_text         TEXT NOT NULL,
    received_at       BIGINT NOT NULL,
    category          TEXT,
    priority          INT,
    title             TEXT,
    linear_issue_url  TEXT,
    status            TEXT NOT NULL,       -- 'received' | 'routed' | 'failed'
    error             TEXT,
    created_at        BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_support_tickets_received_at
    ON support_tickets (received_at DESC);

CREATE INDEX IF NOT EXISTS idx_support_tickets_status
    ON support_tickets (status);
