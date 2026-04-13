package db

import (
	"context"
	"fmt"
)

// SupportTicket is one row of the support_tickets table. Only the fields
// populated at insert time are required; the rest are filled in by the
// async triage pipeline.
type SupportTicket struct {
	ID         string // svix-id
	FromEmail  string
	Subject    string
	BodyText   string
	ReceivedAt int64
}

// InsertSupportTicket inserts a new ticket keyed on svix-id. Returns
// inserted=false when a row with that id already exists (redelivery).
// Callers can treat inserted=false as "nothing to do, ack the webhook".
func (db *DB) InsertSupportTicket(ctx context.Context, t SupportTicket) (bool, error) {
	now := nowUnix()
	cmd, err := db.pool.Exec(ctx,
		`INSERT INTO support_tickets
		     (id, from_email, subject, body_text, received_at, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, 'received', $6)
		 ON CONFLICT (id) DO NOTHING`,
		t.ID, t.FromEmail, t.Subject, t.BodyText, t.ReceivedAt, now,
	)
	if err != nil {
		return false, fmt.Errorf("insert support ticket: %w", err)
	}
	return cmd.RowsAffected() == 1, nil
}

// UpdateTicketRouted records a successful triage + Linear routing.
func (db *DB) UpdateTicketRouted(ctx context.Context, id, category string, priority int, title, linearURL string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE support_tickets
		 SET category = $1,
		     priority = $2,
		     title = $3,
		     linear_issue_url = $4,
		     status = 'routed',
		     error = NULL
		 WHERE id = $5`,
		category, priority, title, linearURL, id,
	)
	if err != nil {
		return fmt.Errorf("update support ticket (routed): %w", err)
	}
	return nil
}

// UpdateTicketFailed records that routing failed. The raw error message
// is stored so operators can inspect it without digging through logs.
func (db *DB) UpdateTicketFailed(ctx context.Context, id, errMsg string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE support_tickets
		 SET status = 'failed',
		     error = $1
		 WHERE id = $2`,
		errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("update support ticket (failed): %w", err)
	}
	return nil
}
