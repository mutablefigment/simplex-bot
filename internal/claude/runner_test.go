package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"claude-bot/internal/config"
)

func newTestRunner() *execRunner {
	return &execRunner{
		cfg: config.Claude{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Issue #31: parseStream must drain stdout to EOF BEFORE cmd.Wait() reaps the
// process, otherwise Wait closes the read end of the stdout pipe and discards
// any still-buffered bytes — typically claude's final `result` frame, which
// carries cost/duration and the organic terminal result. Dropping it makes
// supervise synthesise `ErrCrash: no result event` for a turn that actually
// succeeded.
//
// This test runs a subprocess that, in one go, writes a large blob of leading
// frames (much larger than a pipe buffer so bytes are guaranteed still buffered
// and unread at exit time) followed by the terminal `result` frame, then exits
// immediately. With the read-then-reap fix, supervise surfaces the organic
// result (cost 0.07, duration 4321, no error). With the old reap-then-read order
// the trailing result frame is racily dropped and supervise reports ErrCrash.
func TestSupervise_FinalResultNotDroppedOnImmediateExit(t *testing.T) {
	r := newTestRunner()

	// ~512 lines of assistant frames (well over Linux's 64KiB pipe buffer once
	// each line is padded) then the result frame. The payload is written to a
	// temp file and the child cats it and exits immediately, so the bulk of the
	// data — including the trailing result frame — is guaranteed still buffered
	// in the pipe at the instant the child exits. That is exactly when the old
	// code's cmd.Wait() would close the read end and drop the tail.
	pad := strings.Repeat("y", 600)
	var b strings.Builder
	for i := 0; i < 512; i++ {
		fmt.Fprintf(&b, `{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}`+"\n", pad)
	}
	b.WriteString(`{"type":"result","subtype":"success","is_error":false,"duration_ms":4321,"total_cost_usd":0.07,"result":"done"}` + "\n")

	payload := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(payload, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec cat "+shQuote(payload))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrTail := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = stderrTail
	cmd.WaitDelay = sigtermGrace
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	out := make(chan Event, 512)
	done := make(chan struct{})
	go func() {
		r.supervise(ctx, cmd, stdout, stderrTail, out)
		close(done)
	}()

	var terminal *ResultEvent
	for ev := range out {
		if re, ok := ev.(ResultEvent); ok {
			re := re
			terminal = &re
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not return")
	}

	if terminal == nil {
		t.Fatal("no terminal ResultEvent emitted")
	}
	if terminal.Error != nil {
		t.Fatalf("terminal result has error %v; the final result frame was dropped "+
			"(issue #31: cmd.Wait() reaped the process before parseStream drained stdout)", terminal.Error)
	}
	if terminal.CostUSD != 0.07 || terminal.DurationMS != 4321 {
		t.Errorf("terminal result = %+v, want CostUSD=0.07 DurationMS=4321 (organic result frame lost)", *terminal)
	}
}

// Issue #31 guard: when stdout never reaches EOF (e.g. an orphaned grandchild
// keeps the write-end open, or claude ignores SIGTERM) parseStream's Read blocks
// indefinitely. Read-then-reap would then hang forever in wg.Wait(). The
// closeWatchdog guard must force-close stdout shortly after ctx is cancelled so
// the Read unblocks and supervise returns promptly — without waiting on the
// subprocess to exit.
//
// This test uses the exact adversary the issue calls out: the spawned process
// (the "child") forks a backgrounded grandchild that holds the stdout write-end
// open via a long sleep, then the CHILD EXITS IMMEDIATELY. Because the grandchild
// (reparented to init) still holds the write-end, the pipe never reaches EOF even
// though the child is gone and reaped — so SIGTERM/SIGKILL of the child cannot
// unblock the Read. Verified empirically: the raw Read stays blocked for 8s+ and
// only stdout.Close() releases it. The watchdog's force-close is therefore the
// SOLE thing that lets supervise return. With the guard removed this test hangs
// until the package test timeout.
//
// To isolate OUR watchdog as the load-bearing guard, cmd.Stderr is a real
// *os.File (/dev/null). With a non-File stderr, os/exec spawns a stderr-copy
// goroutine and its WaitDelay path also closes the stdout read end as a side
// effect — which would mask our watchdog. A File stderr means no copy goroutine,
// so os/exec never closes the stdout pipe on its own; only our watchdog does.
// (Production's cmd.Stderr is a tailBuffer, so in production os/exec's WaitDelay
// is an additional, slower backstop; the watchdog is the fast, always-present
// guarantee and the one that also covers the File-stderr / WaitDelay==0 cases.)
func TestSupervise_WedgedStdoutUnblocksOnCancel(t *testing.T) {
	r := newTestRunner()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// "sleep 30 &" = orphaned grandchild keeps stdout open; "exit 0" = child dies
	// at once and is reaped, yet the pipe stays open. No terminating frame is ever
	// written, so parseStream blocks forever in Read absent the watchdog.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & exit 0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devnull.Close()
	cmd.Stderr = devnull // File stderr: no os/exec stdout backstop (see comment)
	// stderrTail is what supervise reads at the end; it need not be cmd.Stderr.
	stderrTail := &tailBuffer{max: stderrTailBytes}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = sigtermGrace // production value; cmd.Wait() still returns fast here
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	out := make(chan Event, eventBuffer)
	go func() {
		for range out {
		}
	}()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		r.supervise(ctx, cmd, stdout, stderrTail, out)
		close(done)
	}()

	// Let the parser get into its blocking Read, then cancel the turn.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// supervise must return bounded by the watchdog (~readGrace after cancel it
	// force-closes stdout, parseStream returns, and cmd.Wait() returns at once
	// since the child already exited and there is no wedged stderr copy goroutine)
	// — NOT by the grandchild's 30s lifetime. Without the watchdog the parser
	// never returns and supervise deadlocks in wg.Wait() until the package test
	// timeout fires.
	bound := readGrace + 3*time.Second
	select {
	case <-done:
		if el := time.Since(start); el > bound {
			t.Errorf("supervise took %v to return after cancel (want < %v); watchdog "+
				"did not promptly unblock the wedged stdout Read", el, bound)
		}
	case <-time.After(bound):
		t.Fatal("supervise hung: wedged stdout Read (grandchild holds write-end) was " +
			"never unblocked after ctx cancel (issue #31 guard missing); read-then-reap " +
			"deadlocked in wg.Wait()")
	}
}

// shQuote wraps s in single quotes for safe use in `sh -c`, escaping embedded
// single quotes. Used to feed a literal payload path to a child command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
