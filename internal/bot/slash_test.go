package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"claude-bot/internal/config"
	"claude-bot/internal/store"
)

// statusText and costText (issue #7) only touch the store + version, so we
// drive them with the real sqlite store and a nil simplex/runner.
func newBotForSlashTest(t *testing.T, version string) *Bot {
	t.Helper()
	st := newTestStore(t)
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, log, version, nil, nil, st)
}

func TestStatusText_NoHistory(t *testing.T) {
	b := newBotForSlashTest(t, "v1.2.3")
	got := b.statusText(context.Background())
	if !strings.Contains(got, "version: v1.2.3") {
		t.Errorf("missing version line: %q", got)
	}
	if !strings.Contains(got, "session: (none") {
		t.Errorf("expected no-session message, got %q", got)
	}
	if !strings.Contains(got, "last turn: (none)") {
		t.Errorf("expected no-turn message, got %q", got)
	}
}

func TestStatusText_WithSessionAndTurn(t *testing.T) {
	b := newBotForSlashTest(t, "")
	ctx := context.Background()

	if err := b.store.SetSessionID(ctx, "abc12345-rest-of-uuid-here"); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	id, err := b.store.InsertTurn(ctx, store.Turn{
		StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Status:    "running",
	})
	if err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if err := b.store.UpdateTurn(ctx, store.Turn{
		ID:        id,
		SessionID: "abc12345-rest-of-uuid-here",
		EndedAt:   time.Date(2026, 1, 2, 3, 4, 10, 0, time.UTC),
		Status:    "ok",
		CostUSD:   0.0123,
	}); err != nil {
		t.Fatalf("UpdateTurn: %v", err)
	}

	got := b.statusText(ctx)
	if !strings.Contains(got, "session: abc12345") {
		t.Errorf("expected first 8 chars of session id, got %q", got)
	}
	if strings.Contains(got, "rest-of-uuid") {
		t.Errorf("full session id leaked: %q", got)
	}
	if !strings.Contains(got, "last turn: ok at 2026-01-02 03:04:10Z") {
		t.Errorf("expected formatted last turn line, got %q", got)
	}
}

func TestCostText(t *testing.T) {
	b := newBotForSlashTest(t, "")
	ctx := context.Background()

	got := b.costText(ctx)
	if got != "$0.0000 total over 0 turns" {
		t.Errorf("empty cost = %q", got)
	}

	for _, c := range []float64{0.0100, 0.0250, 0.0017} {
		id, err := b.store.InsertTurn(ctx, store.Turn{StartedAt: time.Now(), Status: "ok"})
		if err != nil {
			t.Fatalf("InsertTurn: %v", err)
		}
		if err := b.store.UpdateTurn(ctx, store.Turn{
			ID: id, EndedAt: time.Now(), Status: "ok", CostUSD: c,
		}); err != nil {
			t.Fatalf("UpdateTurn: %v", err)
		}
	}
	got = b.costText(ctx)
	if got != "$0.0367 total over 3 turns" {
		t.Errorf("cost after 3 turns = %q", got)
	}
}

func TestHelpTextListsAllCommands(t *testing.T) {
	for _, cmd := range []string{"/new", "/stop", "/status", "/cost", "/help"} {
		if !strings.Contains(helpText, cmd) {
			t.Errorf("helpText missing %s", cmd)
		}
	}
}
