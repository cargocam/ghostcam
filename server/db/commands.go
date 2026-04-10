package db

import (
	"context"
	"encoding/json"
	"fmt"
)

func (db *DB) EnqueueCommand(ctx context.Context, deviceID string, command json.RawMessage) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO camera_commands (device_id, command, created_at) VALUES ($1, $2, $3)`,
		deviceID, command, now)
	if err != nil {
		return fmt.Errorf("enqueue command: %w", err)
	}
	return nil
}

// ClaimCommands atomically marks unclaimed commands for the device as claimed
// and returns them. Claimed rows are deleted in the same statement so the
// commands table cannot grow without bound.
func (db *DB) ClaimCommands(ctx context.Context, deviceID string) ([]json.RawMessage, error) {
	rows, err := db.pool.Query(ctx,
		`DELETE FROM camera_commands
		 WHERE device_id = $1 AND claimed_at IS NULL
		 RETURNING command`,
		deviceID)
	if err != nil {
		return nil, fmt.Errorf("claim commands: %w", err)
	}
	defer rows.Close()

	var commands []json.RawMessage
	for rows.Next() {
		var cmd json.RawMessage
		if err := rows.Scan(&cmd); err != nil {
			return nil, fmt.Errorf("scanning command: %w", err)
		}
		commands = append(commands, cmd)
	}
	return commands, rows.Err()
}
