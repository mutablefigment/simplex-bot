package store

import (
	"context"
	"database/sql"
	"fmt"
)

var migrations = []struct {
	Version int
	SQL     string
}{
	{
		Version: 1,
		SQL: `
CREATE TABLE bot_state (
    key   TEXT PRIMARY KEY,
    value TEXT
);
CREATE TABLE live_messages (
    item_id    INTEGER PRIMARY KEY,
    contact_id INTEGER NOT NULL,
    finalised  INTEGER NOT NULL DEFAULT 0,
    started_at TEXT    NOT NULL
);
CREATE TABLE turns (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    cost_usd   REAL,
    status     TEXT,
    error      TEXT
);
`,
	},
}

func applyMigrations(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.Version, err)
		}
		// PRAGMA user_version doesn't accept parameters; interpolate the int.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set user_version %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
		current = m.Version
	}
	return nil
}
