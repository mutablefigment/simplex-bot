package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteStore struct {
	db *sql.DB
}

func Open(ctx context.Context, dbPath string) (Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	// busy_timeout: avoid SQLITE_BUSY when the worker writes (live_messages,
	// turns, bot_state) and a slash command reads concurrently.
	// WAL: readers don't block writers and vice versa.
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

const sessionKey = "current_session_id"

func (s *sqliteStore) GetSessionID(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM bot_state WHERE key = ?`, sessionKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func (s *sqliteStore) SetSessionID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bot_state(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		sessionKey, id)
	return err
}

func (s *sqliteStore) ClearSession(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bot_state WHERE key = ?`, sessionKey)
	return err
}

func (s *sqliteStore) InsertLiveMessage(ctx context.Context, lm LiveMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO live_messages(item_id, contact_id, finalised, started_at) VALUES(?, ?, ?, ?)`,
		lm.ItemID, lm.ContactID, boolToInt(lm.Finalised), lm.StartedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *sqliteStore) FinaliseLiveMessage(ctx context.Context, itemID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE live_messages SET finalised = 1 WHERE item_id = ?`, itemID)
	return err
}

func (s *sqliteStore) UnfinalisedLiveMessages(ctx context.Context) ([]LiveMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id, contact_id, finalised, started_at FROM live_messages WHERE finalised = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LiveMessage
	for rows.Next() {
		var (
			lm        LiveMessage
			finalised int
			startedAt string
		)
		if err := rows.Scan(&lm.ItemID, &lm.ContactID, &finalised, &startedAt); err != nil {
			return nil, err
		}
		lm.Finalised = finalised != 0
		t, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse started_at: %w", err)
		}
		lm.StartedAt = t
		out = append(out, lm)
	}
	return out, rows.Err()
}

func (s *sqliteStore) InsertTurn(ctx context.Context, t Turn) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO turns(session_id, started_at, ended_at, cost_usd, status, error)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		nullableString(t.SessionID),
		t.StartedAt.UTC().Format(time.RFC3339Nano),
		nullableTime(t.EndedAt),
		t.CostUSD,
		nullableString(t.Status),
		nullableString(t.Error),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *sqliteStore) UpdateTurn(ctx context.Context, t Turn) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns SET session_id = ?, ended_at = ?, cost_usd = ?, status = ?, error = ?
		 WHERE id = ?`,
		nullableString(t.SessionID),
		nullableTime(t.EndedAt),
		t.CostUSD,
		nullableString(t.Status),
		nullableString(t.Error),
		t.ID,
	)
	return err
}

func (s *sqliteStore) MarkStaleRunningTurns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status = 'cancelled', ended_at = ? WHERE status = 'running'`,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *sqliteStore) TotalCost(ctx context.Context) (float64, error) {
	var v sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM turns`).Scan(&v); err != nil {
		return 0, err
	}
	return v.Float64, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
