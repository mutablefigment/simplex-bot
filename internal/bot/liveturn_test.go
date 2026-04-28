package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"claude-bot/internal/store"
)

type senderCall struct {
	op        string // send_live | update_live | finalise
	contactID int64
	itemID    int64
	text      string
	quotedID  int64
}

type fakeSender struct {
	nextID      int64
	calls       []senderCall
	sendLiveErr error
	updateErr   error
	finaliseErr error
}

func (f *fakeSender) SendLive(ctx context.Context, cid int64, text string, qid int64) (int64, error) {
	if f.sendLiveErr != nil {
		return 0, f.sendLiveErr
	}
	f.nextID++
	f.calls = append(f.calls, senderCall{
		op: "send_live", contactID: cid, itemID: f.nextID, text: text, quotedID: qid,
	})
	return f.nextID, nil
}

func (f *fakeSender) UpdateLive(ctx context.Context, cid, iid int64, text string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.calls = append(f.calls, senderCall{
		op: "update_live", contactID: cid, itemID: iid, text: text,
	})
	return nil
}

func (f *fakeSender) Finalise(ctx context.Context, cid, iid int64, text string) error {
	if f.finaliseErr != nil {
		return f.finaliseErr
	}
	f.calls = append(f.calls, senderCall{
		op: "finalise", contactID: cid, itemID: iid, text: text,
	})
	return nil
}

func opSequence(calls []senderCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.op
	}
	return out
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newLT(t *testing.T, sender liveSender, threshold int) (*LiveTurn, store.Store) {
	t.Helper()
	st := newTestStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lt := newLiveTurn(log, sender, st, 42, 7, threshold)
	return lt, st
}

func TestLiveTurn_LazyOpenOnFirstFlush(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	// no Append yet → Flush is no-op
	if err := lt.Flush(ctx); err != nil {
		t.Fatalf("Flush empty: %v", err)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("Flush on empty buf made %d calls, want 0", len(sender.calls))
	}

	lt.Append("hello")
	if err := lt.Flush(ctx); err != nil {
		t.Fatalf("Flush after Append: %v", err)
	}

	if len(sender.calls) != 1 || sender.calls[0].op != "send_live" {
		t.Fatalf("expected single send_live, got %+v", sender.calls)
	}
	if sender.calls[0].text != "hello" {
		t.Errorf("text = %q, want %q", sender.calls[0].text, "hello")
	}
	if sender.calls[0].contactID != 42 {
		t.Errorf("contactID = %d, want 42", sender.calls[0].contactID)
	}
	if sender.calls[0].quotedID != 7 {
		t.Errorf("quotedID = %d, want 7 (prompt quote on first send)", sender.calls[0].quotedID)
	}
}

func TestLiveTurn_FinaliseEmptyReturnsNotSent(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	sent, err := lt.Finalise(ctx, "")
	if err != nil {
		t.Fatalf("Finalise empty: %v", err)
	}
	if sent {
		t.Errorf("sent = true, want false (no live ever opened)")
	}
	if len(sender.calls) != 0 {
		t.Errorf("got %d sender calls on empty turn, want 0", len(sender.calls))
	}
}

func TestLiveTurn_NoOpFlushWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	lt.Append("hello")
	_ = lt.Flush(ctx)
	_ = lt.Flush(ctx) // same buffer — no UpdateLive
	_ = lt.Flush(ctx)

	if got := opSequence(sender.calls); len(got) != 1 || got[0] != "send_live" {
		t.Fatalf("ops = %v, want [send_live] only", got)
	}
}

func TestLiveTurn_BasicStreamingFlow(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	lt.Append("Hello")
	_ = lt.Flush(ctx)
	lt.Append(" world")
	_ = lt.Flush(ctx)
	lt.Append("!")
	sent, err := lt.Finalise(ctx, "")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if !sent {
		t.Errorf("sent = false, want true")
	}

	want := []string{"send_live", "update_live", "finalise"}
	if got := opSequence(sender.calls); !equalStrs(got, want) {
		t.Fatalf("ops = %v, want %v", got, want)
	}
	last := sender.calls[len(sender.calls)-1]
	if last.text != "Hello world!" {
		t.Errorf("final text = %q, want %q", last.text, "Hello world!")
	}
}

func TestLiveTurn_MarkdownAppliedAtFlush(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	// Bold marker arrives in two events — must only translate once both halves
	// have arrived (handled implicitly by full-buffer translation per flush).
	lt.Append("Say **bo")
	_ = lt.Flush(ctx)
	if got := sender.calls[len(sender.calls)-1].text; got != "Say **bo" {
		t.Errorf("partial bold should be left raw: got %q", got)
	}

	lt.Append("ld** now")
	_ = lt.Flush(ctx)
	if got := sender.calls[len(sender.calls)-1].text; got != "Say *bold* now" {
		t.Errorf("complete bold should translate: got %q", got)
	}
}

func TestLiveTurn_SuffixAppendedInFinalise(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	lt.Append("answer")
	_ = lt.Flush(ctx)
	_, _ = lt.Finalise(ctx, "— $0.0042 · 3.1s")

	last := sender.calls[len(sender.calls)-1]
	if last.op != "finalise" {
		t.Fatalf("last op = %q, want finalise", last.op)
	}
	if last.text != "answer\n\n— $0.0042 · 3.1s" {
		t.Errorf("final text = %q (separator should be inserted by FSM)", last.text)
	}
}

func TestLiveTurn_FinaliseTextForFallback(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 0)

	// Empty turn with cost footer: never opened, Finalise stashes the suffix
	// alone (no separator since text is empty).
	sent, _ := lt.Finalise(ctx, "— $0.0001 · 0.5s")
	if sent {
		t.Errorf("sent = true, want false")
	}
	if got := lt.FinaliseText(); got != "— $0.0001 · 0.5s" {
		t.Errorf("FinaliseText = %q", got)
	}

	// Reset for another scenario: text + suffix → joined with \n\n.
	sender2 := &fakeSender{}
	lt2, _ := newLT(t, sender2, 0)
	lt2.Append("body")
	sent2, _ := lt2.Finalise(ctx, "⚠️ interrupted")
	if sent2 {
		t.Errorf("sent = true (no flush ran, no item open) — want false")
	}
	if got := lt2.FinaliseText(); got != "body\n\n⚠️ interrupted" {
		t.Errorf("FinaliseText = %q", got)
	}
}

func TestLiveTurn_RotationAtThreshold(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 50) // tiny threshold

	// First flush stays under threshold.
	lt.Append("first chunk")
	_ = lt.Flush(ctx)

	// Second flush crosses threshold → rotation: update + finalise (no quote
	// next time), itemID resets, buffer reset.
	lt.Append(strings.Repeat("x", 60))
	_ = lt.Flush(ctx)

	// Now Append more — should open a NEW live message (no quote).
	lt.Append("second message text")
	_ = lt.Flush(ctx)

	// Finalise the second message.
	_, _ = lt.Finalise(ctx, "")

	want := []string{
		"send_live",   // first item, quoted
		"update_live", // crosses threshold
		"finalise",    // rotation closes first item
		"send_live",   // second item, no quote
		"finalise",    // closes second
	}
	got := opSequence(sender.calls)
	if !equalStrs(got, want) {
		t.Fatalf("ops = %v\nwant %v", got, want)
	}

	// First send quoted; second did not.
	first := sender.calls[0]
	secondSend := sender.calls[3]
	if first.quotedID != 7 {
		t.Errorf("first quotedID = %d, want 7", first.quotedID)
	}
	if secondSend.quotedID != 0 {
		t.Errorf("second send quotedID = %d, want 0 (no quote on rotated msgs)", secondSend.quotedID)
	}
	if first.itemID == secondSend.itemID {
		t.Errorf("rotated send reused itemID %d", first.itemID)
	}
}

func TestLiveTurn_StoreInsertsAndFinalises(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, st := newLT(t, sender, 0)

	lt.Append("hi")
	_ = lt.Flush(ctx)

	open, err := st.UnfinalisedLiveMessages(ctx)
	if err != nil {
		t.Fatalf("UnfinalisedLiveMessages: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("after flush: open=%d, want 1", len(open))
	}

	_, _ = lt.Finalise(ctx, "")
	open, _ = st.UnfinalisedLiveMessages(ctx)
	if len(open) != 0 {
		t.Errorf("after finalise: open=%d, want 0", len(open))
	}
}

func TestLiveTurn_FinaliseAfterRotateOnlyReturnsNotSent(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{}
	lt, _ := newLT(t, sender, 30)

	// Trigger a rotation so itemID resets.
	lt.Append(strings.Repeat("y", 40))
	_ = lt.Flush(ctx)

	// No further Append → Finalise sees itemID=0.
	sent, err := lt.Finalise(ctx, "")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if sent {
		t.Errorf("sent = true after rotation w/ no follow-up text, want false")
	}
}

func TestLiveTurn_SendLiveErrorPropagates(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{sendLiveErr: errors.New("boom")}
	lt, _ := newLT(t, sender, 0)

	lt.Append("x")
	err := lt.Flush(ctx)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "SendLive") {
		t.Errorf("error did not name SendLive: %v", err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
