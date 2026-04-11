package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies each migration file under migrations/ in filename
// order exactly once per database. Applied migrations are recorded in the
// schema_migrations table; subsequent runs skip anything already tracked.
//
// On the first run after upgrading from the untracked world, schema_migrations
// is empty and every bundled migration is re-executed. That's safe as long as
// each individual migration is idempotent (CREATE … IF NOT EXISTS, ADD COLUMN
// IF NOT EXISTS, DO blocks that guard DDL on still-existent objects, etc.).
// Any new migration added to the repo must preserve that property until every
// live database has schema_migrations populated.
func (db *DB) RunMigrations(ctx context.Context) error {
	if _, err := db.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("reading migrations directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		var applied bool
		if err := db.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("checking migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		slog.Info("running migration", "file", name)
		if _, err := db.pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("executing migration %s: %w", name, err)
		}

		if _, err := db.pool.Exec(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
			name, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
	}

	return nil
}
