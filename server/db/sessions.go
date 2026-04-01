package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const sessionTTLDays = 30

func (db *PostgresDB) CreateSession(ctx context.Context, session *NewSession) error {
	now := nowUnix()
	expiresAt := now + 86400*sessionTTLDays
	_, err := db.pool.Exec(ctx,
		`INSERT INTO sessions (session_id, user_id, created_at, expires_at, user_agent, ip_address)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		session.SessionID, session.UserID, now, expiresAt, nilIfEmpty(session.UserAgent), nilIfEmpty(session.IPAddress))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (db *PostgresDB) GetSession(ctx context.Context, sessionID string) (*SessionRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT session_id, user_id, created_at, expires_at, last_active_at
		 FROM sessions WHERE session_id = $1`, sessionID)

	var s SessionRecord
	err := row.Scan(&s.SessionID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastActiveAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &s, nil
}

func (db *PostgresDB) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM sessions WHERE session_id = $1", sessionID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (db *PostgresDB) ExtendSession(ctx context.Context, sessionID string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		"UPDATE sessions SET last_active_at = $1 WHERE session_id = $2", now, sessionID)
	if err != nil {
		return fmt.Errorf("extend session: %w", err)
	}
	return nil
}

func (db *PostgresDB) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	now := nowUnix()
	ct, err := db.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < $1", now)
	if err != nil {
		return 0, fmt.Errorf("cleanup sessions: %w", err)
	}
	return ct.RowsAffected(), nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
