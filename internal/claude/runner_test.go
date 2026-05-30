package claude

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"claude-bot/internal/config"
)

// Issue #4: stderr must never bleed into the returned error string.
// classifyStderr inspects stderr to choose a typed error but must not embed it.
func TestClassifyStderr_NoLeak(t *testing.T) {
	secret := "sk-ant-api03-abc123-DEF456-secret-leak-marker"
	base := errors.New("claude: crash")

	tests := []struct {
		name   string
		stderr string
		want   error
	}{
		{"rate_limit", "rate limit reached " + secret, ErrRateLimit},
		{"rate_limit_underscore", "Error: rate_limit exceeded — " + secret, ErrRateLimit},
		{"auth_unauthorized", "Unauthorized: " + secret, ErrAuth},
		{"auth_authentication", "Authentication failed: " + secret, ErrAuth},
		{"auth_api_key", "invalid api key " + secret, ErrAuth},
		{"unclassified", "panic in subprocess at /tmp/" + secret, base},
		{"empty", "", base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStderr(base, tt.stderr)
			if !errors.Is(got, tt.want) {
				t.Errorf("classifyStderr error type = %v, want %v", got, tt.want)
			}
			if strings.Contains(got.Error(), "sk-ant") || strings.Contains(got.Error(), "secret-leak-marker") {
				t.Errorf("stderr leaked into error message: %q", got.Error())
			}
		})
	}
}

// Issue #27: supervise's terminal ResultEvent send must be context-aware so it
// cannot leak the supervise goroutine when the consumer stops draining `out`.
//
// In production runTurn (internal/bot) breaks out of its read loop the instant
// the turn ctx is cancelled (/stop or turn timeout) and never reads `events`
// again. The `out` channel is buffered eventBuffer (32). If 32 events are
// already buffered and ctx is cancelled, the pre-fix bare `out <- terminal`
// blocked forever against the full channel + dead consumer, leaking supervise
// (plus the retained channel and stderrTail) per cancelled/timed-out turn.
//
// This test reproduces that exact shape: pre-fill `out` to capacity, cancel the
// ctx, never drain `out`, then run supervise. With the ctx-aware select it
// returns and its deferred close(out) fires; without the fix supervise blocks on
// the terminal send and `out` is never closed, so the test times out (fails).
func TestSupervise_TerminalSendIsCtxAware(t *testing.T) {
	r := &execRunner{
		cfg: config.Claude{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// A trivial subprocess that exits cleanly and immediately. We don't need real
	// claude output; supervise's terminal send is what we're exercising. ctx is
	// cancelled below, so terminalResult yields a context-error ResultEvent.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/true")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrTail := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = stderrTail
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Fill the event buffer to capacity so the terminal send has nowhere to go,
	// mirroring a turn that produced >= eventBuffer events before cancellation.
	out := make(chan Event, eventBuffer)
	for i := 0; i < eventBuffer; i++ {
		out <- AssistantTextEvent{Text: "x"}
	}

	// Consumer is gone: cancel the turn and never read `out` again.
	cancel()

	done := make(chan struct{})
	go func() {
		r.supervise(ctx, cmd, stdout, stderrTail, out)
		close(done)
	}()

	// supervise closes `out` via its deferred close on return; observe completion
	// without draining the buffered events (we never read from `out`).
	select {
	case <-done:
		// supervise returned: no leak.
	case <-time.After(5 * time.Second):
		t.Fatal("supervise leaked: terminal ResultEvent send blocked on a full " +
			"channel with a dead consumer (issue #27); it did not return after ctx cancel")
	}

	// And the channel must actually be closed (the contract: out is closed after
	// the terminal ResultEvent, even when nobody read it).
	drain := make(chan struct{})
	go func() {
		for range out {
		}
		close(drain)
	}()
	select {
	case <-drain:
	case <-time.After(time.Second):
		t.Fatal("out channel was not closed after supervise returned")
	}
}
