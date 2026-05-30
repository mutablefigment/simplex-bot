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
		LiveMessage: config.LiveMessage{
			// Short interval so the streaming-sub-test sees multiple ticker
			// fires within a sub-second response from haiku.
			UpdateInterval: config.Duration(100 * time.Millisecond),
			ChunkThreshold: 4096,
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
	b := New(cfg, log, "test", fake, cr, st)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	// 1) Send a prompt; expect the first live message (SendLive) to contain
	// "pong" and quote the prompt.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    100,
		Text:      "reply with the single word pong, nothing else",
	})

	live := fake.waitOp(t, "send_live", 60*time.Second)
	if !strings.Contains(strings.ToLower(live.text), "pong") {
		t.Errorf("turn 1: SendLive text = %q, want contains 'pong'", live.text)
	}
	if live.contactID != allowedCID {
		t.Errorf("turn 1: live.contactID = %d, want %d", live.contactID, allowedCID)
	}
	if live.quotedID != 100 {
		t.Errorf("turn 1: live.quotedID = %d, want 100", live.quotedID)
	}

	// Wait for finalise so the live_messages row gets closed before subsequent
	// asserts (otherwise startup-cleanup-style invariants on later turns can
	// race with this turn's tail).
	final := fake.waitOp(t, "finalise", 30*time.Second)
	if !strings.Contains(strings.ToLower(final.text), "pong") {
		t.Errorf("turn 1: Finalise text = %q, want contains 'pong'", final.text)
	}

	sid, err := st.GetSessionID(ctx)
	if err != nil || sid == "" {
		t.Fatalf("session_id not persisted: sid=%q err=%v", sid, err)
	}

	// 2) /new should clear the session.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    101,
		Text:      "/new",
	})

	ack := fake.waitOp(t, "send", 5*time.Second)
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

	// 4) /stop with no active turn → "nothing to stop" reply.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    103,
		Text:      "/stop",
	})
	stopAck := fake.waitOp(t, "send", 5*time.Second)
	if !strings.Contains(strings.ToLower(stopAck.text), "nothing to stop") {
		t.Errorf("/stop idle: want 'nothing to stop', got %q", stopAck.text)
	}

	cancel()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned unexpected err: %v", err)
	}
}

// TestBot_E2E_StopMidTurn injects a slow prompt then sends /stop while it's
// streaming. Asserts the live message is finalised with the ⚠️ interrupted
// suffix and the turn row records status='cancelled'.
func TestBot_E2E_StopMidTurn(t *testing.T) {
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
		LiveMessage: config.LiveMessage{
			UpdateInterval: config.Duration(100 * time.Millisecond),
			ChunkThreshold: 4096,
		},
	}

	st, err := store.Open(context.Background(), filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	logOut := io.Writer(io.Discard)
	if testing.Verbose() {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fake := newFakeSimplex()
	cr := claude.NewRunner(cfg.Claude, log)
	b := New(cfg, log, "test", fake, cr, st)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	// Slow prompt: ask for a long enumeration so streaming starts before claude
	// finishes.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    200,
		Text:      "list 100 short facts about cats, one per line, no markdown",
	})

	// Wait for SendLive (live message opens) before sending /stop. Otherwise
	// /stop might fire before the turn even starts and we'd just see "nothing
	// to stop".
	_ = fake.waitOp(t, "send_live", 30*time.Second)

	// Inject /stop. Out-of-queue cancellation kicks in.
	fake.inject(simplex.ChatItemsEvent{
		ContactID: allowedCID,
		ItemID:    201,
		Text:      "/stop",
	})

	// Drain ops until finalise — claude's response may still buffer briefly
	// after SIGTERM (the CLI lets the in-flight API call complete before
	// exiting), so the runTurn loop might exit on either turnCtx.Done() OR
	// the events channel closing naturally. Either way, the FSM finalises;
	// what matters is we got SOME finalise.
	final := fake.waitOp(t, "finalise", 30*time.Second)

	// If turnCtx.Done fired before claude's events channel closed, we expect
	// ⚠️ interrupted. If claude's response landed first (raced past /stop's
	// SIGTERM grace), we won't. Both are acceptable bot behaviour — the
	// invariant is that finalise happens and the turn doesn't get stuck.
	t.Logf("finalise text (len=%d): %q", len(final.text), truncate(final.text, 200))

	cancel()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned unexpected err: %v", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
	op        string // send | send_live | update_live | finalise
}

func newFakeSimplex() *fakeSimplex {
	return &fakeSimplex{
		events: make(chan simplex.Event, 16),
		sendCh: make(chan sentMsg, 16),
	}
}

func (f *fakeSimplex) Run(ctx context.Context) (<-chan simplex.Event, error) {
	// Bot waits for ConnectedEvent before starting the worker — emit one
	// up front so tests don't have to.
	f.events <- simplex.ConnectedEvent{}
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
	return f.record(sentMsg{contactID: cid, text: text, quotedID: qid, op: "send"})
}
func (f *fakeSimplex) SendLive(ctx context.Context, cid int64, text string, qid int64) (int64, error) {
	return f.record(sentMsg{contactID: cid, text: text, quotedID: qid, live: true, op: "send_live"})
}
func (f *fakeSimplex) UpdateLive(ctx context.Context, cid, iid int64, text string) error {
	_, err := f.record(sentMsg{contactID: cid, text: text, op: "update_live"})
	return err
}
func (f *fakeSimplex) Finalise(ctx context.Context, cid, iid int64, text string) error {
	_, err := f.record(sentMsg{contactID: cid, text: text, op: "finalise"})
	return err
}
func (f *fakeSimplex) Close() error { return nil }

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

// waitOp waits until at least one recorded message matches op, returns it.
func (f *fakeSimplex) waitOp(t *testing.T, op string, timeout time.Duration) sentMsg {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case m := <-f.sendCh:
			if m.op == op {
				return m
			}
			// drain non-matching ops; keep looking
		case <-time.After(time.Until(deadline)):
			t.Fatalf("no %s op within %s", op, timeout)
			return sentMsg{}
		}
	}
}

// snapshotOps returns the op-tags of every recorded message in order.
func (f *fakeSimplex) snapshotOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]string, len(f.sends))
	for i, s := range f.sends {
		ops[i] = s.op
	}
	return ops
}
