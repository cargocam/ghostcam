package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

func (db *DB) InsertAuditEntry(ctx context.Context, timestamp, eventType string, eventData json.RawMessage, hmac string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO audit_log (timestamp, event_type, event_data, hmac)
		 VALUES ($1::timestamptz, $2, $3, $4)`,
		timestamp, eventType, eventData, hmac)
	if err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}

func (db *DB) QueryAuditLog(ctx context.Context, eventType, since, until string, limit, offset int64) ([]AuditLogRecord, int64, error) {
	query := `SELECT id, timestamp::text, event_type, event_data, hmac, COUNT(*) OVER() AS total
		 FROM audit_log WHERE 1=1`
	args := []any{}
	argIdx := 1

	if eventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, eventType)
		argIdx++
	}
	if since != "" {
		query += fmt.Sprintf(" AND timestamp >= $%d::timestamptz", argIdx)
		args = append(args, since)
		argIdx++
	}
	if until != "" {
		query += fmt.Sprintf(" AND timestamp <= $%d::timestamptz", argIdx)
		args = append(args, until)
		argIdx++
	}

	query += " ORDER BY timestamp DESC"
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	var entries []AuditLogRecord
	var total int64
	for rows.Next() {
		var e AuditLogRecord
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.EventType, &e.EventData, &e.HMAC, &total); err != nil {
			return nil, 0, fmt.Errorf("scanning audit entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

func (db *DB) GetHMACSecret(ctx context.Context) ([]byte, error) {
	var secret []byte
	err := db.pool.QueryRow(ctx, "SELECT value FROM config WHERE key = 'hmac_secret'").Scan(&secret)
	if err != nil {
		return nil, fmt.Errorf("get HMAC secret: %w", err)
	}
	return secret, nil
}

func (db *DB) HealthCheck(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, "SELECT 1")
	return err
}

// AuditLog logs an audit event using slog. Structured logs double as the
// audit trail — the audit_log table is only populated by explicit
// InsertAuditEntry calls (e.g. HMAC-chained events).
func AuditLog(eventType string, fields ...any) {
	slog.Info("audit", append([]any{"event_type", eventType}, fields...)...)
}
