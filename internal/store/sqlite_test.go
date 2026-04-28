package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Round-trip a session id to prove the schema is usable.
	if err := st.SetSessionID(ctx, "abc-123"); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	got, err := st.GetSessionID(ctx)
	if err != nil {
		t.Fatalf("GetSessionID: %v", err)
	}
	if got != "abc-123" {
		t.Errorf("session id = %q", got)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening is idempotent.
	st2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer st2.Close()

	got2, err := st2.GetSessionID(ctx)
	if err != nil {
		t.Fatalf("GetSessionID after reopen: %v", err)
	}
	if got2 != "abc-123" {
		t.Errorf("session id after reopen = %q", got2)
	}
}

func TestTurnLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	id, err := st.InsertTurn(ctx, Turn{StartedAt: time.Now(), Status: "running"})
	if err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	if err := st.UpdateTurn(ctx, Turn{
		ID: id, SessionID: "s1", EndedAt: time.Now(), CostUSD: 0.42, Status: "ok",
	}); err != nil {
		t.Fatalf("UpdateTurn: %v", err)
	}

	cost, err := st.TotalCost(ctx)
	if err != nil {
		t.Fatalf("TotalCost: %v", err)
	}
	if cost != 0.42 {
		t.Errorf("total = %v, want 0.42", cost)
	}
}

func TestLiveMessageFinalise(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "lm.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.InsertLiveMessage(ctx, LiveMessage{
		ItemID: 99, ContactID: 1, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertLiveMessage: %v", err)
	}

	pending, err := st.UnfinalisedLiveMessages(ctx)
	if err != nil {
		t.Fatalf("UnfinalisedLiveMessages: %v", err)
	}
	if len(pending) != 1 || pending[0].ItemID != 99 {
		t.Errorf("pending = %+v", pending)
	}

	if err := st.FinaliseLiveMessage(ctx, 99); err != nil {
		t.Fatalf("FinaliseLiveMessage: %v", err)
	}
	pending, err = st.UnfinalisedLiveMessages(ctx)
	if err != nil {
		t.Fatalf("UnfinalisedLiveMessages: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 unfinalised, got %d", len(pending))
	}
}
