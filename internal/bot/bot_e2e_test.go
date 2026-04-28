package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-bot/internal/claude"
	"claude-bot/internal/config"
	"claude-bot/internal/simplex"
	"claude-bot/internal/store"
)

// TestBot_E2E_Live drives the real bot orchestrator with a fake SimpleX client
// and the real `claude` binary. Skipped unless CLAUDE_BOT_INTEGRATION=1 since
// it costs API tokens.
//
// Path under test:
//
//	fake newChatItems → bot whitelist + queue → worker → real claude subprocess
//	→ stream-json parser → bot collects assistant text → fake simplex.Send
//	→ test asserts payload + that session_id was persisted.
func TestBot_E2E_Live(t *testing.T) {
	if os.Getenv("CLAUDE_BOT_INTEGRATION") != "1" {
		t.Skip("set CLAUDE_BOT_INTEGRATION=1 to run (costs API tokens)")
	}
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "/home/sprite/.local/bin/claude"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("claude binary missing at %s", bin)
	}

	const allowedCID = int64(42)
	tmp := t.TempDir()

	cfg := &config.Config{
		Simplex: config.Simplex{AllowedContactID: allowedCID},
		Claude: config.Claude{
			Binary:      bin,
			Workspace:   tmp,
			Model:       "claude-haiku-4-5-20251001",
			TurnTimeout: config.Duration(60 * time.Second),
		},
	}

	st, err := store.Open(context.Background(), filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := newFakeSimplex()
	cr := claude.NewRunner(cfg.Claude, log)
	b := New(cfg, log, fake, cr, st)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	// 1) Send a prompt; expect a reply containing "pong".
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    100,
		Text:      "reply with the single word pong, nothing else",
	})

	reply := fake.waitSend(t, 60*time.Second)
	if !strings.Contains(strings.ToLower(reply.text), "pong") {
		t.Errorf("turn 1: want reply containing 'pong', got %q", reply.text)
	}
	if reply.contactID != allowedCID {
		t.Errorf("turn 1: reply.contactID = %d, want %d", reply.contactID, allowedCID)
	}
	if reply.quotedID != 100 {
		t.Errorf("turn 1: reply.quotedID = %d, want 100", reply.quotedID)
	}

	sid, err := st.GetSessionID(ctx)
	if err != nil || sid == "" {
		t.Fatalf("session_id not persisted: sid=%q err=%v", sid, err)
	}

	// 2) /new should clear the session — even though the previous turn just wrote it.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    101,
		Text:      "/new",
	})

	ack := fake.waitSend(t, 5*time.Second)
	if !strings.Contains(strings.ToLower(ack.text), "session cleared") {
		t.Errorf("/new: want 'session cleared' ack, got %q", ack.text)
	}

	sid2, err := st.GetSessionID(ctx)
	if err != nil {
		t.Fatalf("read session after /new: %v", err)
	}
	if sid2 != "" {
		t.Errorf("/new did not clear session: sid=%q", sid2)
	}

	// 3) Non-whitelisted contact is rejected (no Send fired).
	before := fake.sendCount()
	fake.inject(simplex.ChatItemsEvent{
		ContactID: 999,
		ItemID:    102,
		Text:      "should be ignored",
	})
	time.Sleep(200 * time.Millisecond)
	if fake.sendCount() != before {
		t.Error("non-whitelisted contact triggered a Send")
	}

	cancel()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned unexpected err: %v", err)
	}
}

// fakeSimplex is an in-memory simplex.Client. inject() pushes events to the
// bot; sends are recorded for assertion.
type fakeSimplex struct {
	mu      sync.Mutex
	events  chan simplex.Event
	sends   []sentMsg
	sendCh  chan sentMsg
	closed  bool
}

type sentMsg struct {
	contactID int64
	text      string
	quotedID  int64
	live      bool
}

func newFakeSimplex() *fakeSimplex {
	return &fakeSimplex{
		events: make(chan simplex.Event, 16),
		sendCh: make(chan sentMsg, 16),
	}
}

func (f *fakeSimplex) Run(ctx context.Context) (<-chan simplex.Event, error) {
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		if !f.closed {
			f.closed = true
			close(f.events)
		}
		f.mu.Unlock()
	}()
	return f.events, nil
}

func (f *fakeSimplex) inject(ev simplex.Event) {
	f.events <- ev
}

func (f *fakeSimplex) record(m sentMsg) (int64, error) {
	f.mu.Lock()
	f.sends = append(f.sends, m)
	id := int64(1000 + len(f.sends))
	f.mu.Unlock()
	f.sendCh <- m
	return id, nil
}

func (f *fakeSimplex) Send(ctx context.Context, cid int64, text string, qid int64) (int64, error) {
	return f.record(sentMsg{contactID: cid, text: text, quotedID: qid})
}
func (f *fakeSimplex) SendLive(ctx context.Context, cid int64, text string, qid int64) (int64, error) {
	return f.record(sentMsg{contactID: cid, text: text, quotedID: qid, live: true})
}
func (f *fakeSimplex) UpdateLive(ctx context.Context, cid, iid int64, text string) error { return nil }
func (f *fakeSimplex) Finalise(ctx context.Context, cid, iid int64, text string) error   { return nil }
func (f *fakeSimplex) GetChats(ctx context.Context) ([]simplex.Chat, error)              { return nil, nil }
func (f *fakeSimplex) Close() error                                                       { return nil }

func (f *fakeSimplex) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeSimplex) waitSend(t *testing.T, timeout time.Duration) sentMsg {
	t.Helper()
	select {
	case m := <-f.sendCh:
		return m
	case <-time.After(timeout):
		t.Fatalf("no Send within %s", timeout)
		return sentMsg{}
	}
}
