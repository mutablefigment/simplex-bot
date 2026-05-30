package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"claude-bot/internal/config"
)

const (
	stderrTailBytes = 8 * 1024
	sigtermGrace    = 5 * time.Second
	eventBuffer     = 32

	// readGrace bounds how long the stdout reader (parseStream) is allowed to
	// keep blocking on Read after ctx is done. ctx (the bot's turnCtx) always
	// fires (turn timeout or /stop), and cmd.Cancel sends SIGTERM on that, so a
	// well-behaved claude reaches EOF almost immediately. readGrace is the guard
	// for the adversarial case where EOF never comes — e.g. claude exits but an
	// orphaned grandchild inherited and still holds the stdout write-end open, or
	// claude ignores SIGTERM. After ctx is done + readGrace, the watchdog
	// force-closes stdout to unblock the Read so supervise can return. It is kept
	// shorter than sigtermGrace (cmd.WaitDelay) so the read is unblocked before
	// (or no later than) os/exec's own WaitDelay-driven pipe close, making
	// supervise's return time bounded by readGrace rather than WaitDelay.
	readGrace = 1 * time.Second
)

type Runner interface {
	// Run spawns claude with --resume sessionID (empty = fresh).
	// Contract: ResultEvent ALWAYS fires last (synthesised if the subprocess
	// crashed or was cancelled before emitting one), with Error set on failure.
	// The channel is closed after ResultEvent.
	Run(ctx context.Context, prompt, sessionID string) (<-chan Event, error)
}

func NewRunner(cfg config.Claude, log *slog.Logger) Runner {
	return &execRunner{cfg: cfg, log: log}
}

type execRunner struct {
	cfg config.Claude
	log *slog.Logger
}

func (r *execRunner) Run(ctx context.Context, prompt, sessionID string) (<-chan Event, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", "bypassPermissions",
		"--model", r.cfg.Model,
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if len(r.cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, r.cfg.AllowedTools...)
	}
	if len(r.cfg.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, r.cfg.DisallowedTools...)
	}

	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Dir = r.cfg.Workspace
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrTail := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = stderrTail

	// SIGTERM (not SIGKILL) on ctx cancel so claude can flush.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = sigtermGrace

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	r.log.Info("claude: spawned",
		"pid", cmd.Process.Pid,
		"session", shortSession(sessionID),
		"prompt_bytes", len(prompt))

	out := make(chan Event, eventBuffer)
	go r.supervise(ctx, cmd, stdout, stderrTail, out)
	return out, nil
}

func (r *execRunner) supervise(
	ctx context.Context,
	cmd *exec.Cmd,
	stdout io.ReadCloser,
	stderrTail *tailBuffer,
	out chan<- Event,
) {
	defer close(out)

	// Read-then-reap (issue #31). The previous order called cmd.Wait() *before*
	// parseStream had drained stdout. cmd.Wait() closes the read end of the
	// stdout pipe once it sees the process exit (os/exec owns that fd via
	// StdoutPipe), so any bytes still buffered in the pipe — typically claude's
	// final `result` frame carrying cost/duration/terminal result — were
	// discarded, and the turn was misreported as `ErrCrash: no result event`.
	//
	// Correct order: let parseStream run to EOF first, *then* cmd.Wait(). EOF on
	// the stdout pipe occurs only once every write-end is closed (claude and any
	// child it spawned that inherited the fd), so by the time parseStream returns
	// it has observed every byte claude flushed, including the result frame.
	//
	// The hazard of read-then-reap is a hang: if EOF never comes (an orphaned
	// grandchild keeps the stdout write-end open, or claude ignores SIGTERM and
	// keeps writing), the parse goroutine blocks in Read forever and wg.Wait()
	// below would never return. The watchdog goroutine is the explicit guard
	// against that — see closeWatchdog. We do NOT rely on os/exec's WaitDelay
	// internals to rescue us, because cmd.Wait() is only reached *after* the
	// parser finishes; the watchdog is what guarantees the parser finishes.
	var (
		organic  *ResultEvent
		parseErr error
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		organic, parseErr = parseStream(ctx, stdout, out)
	}()

	// Watchdog: once ctx is done (turnCtx always fires) plus a short grace,
	// force-close stdout so a wedged parseStream Read is unblocked and wg.Wait()
	// can return. stopWatchdog cleans it up on the normal path.
	stopWatchdog := r.closeWatchdog(ctx, stdout)

	// Wait for the reader to drain stdout to EOF (or to be force-unblocked by the
	// watchdog) BEFORE reaping the process, so no buffered bytes are lost.
	wg.Wait()
	stopWatchdog()

	waitErr := cmd.Wait()

	stderrText := stderrTail.String()
	terminal := r.terminalResult(ctx, organic, waitErr, parseErr, stderrText)
	// Log stderr separately and only here: it may contain API key fragments,
	// account IDs, or stack frames. Never propagate it into terminal.Error,
	// which gets rendered into a user-visible reply by the bot.
	if terminal.Error != nil && stderrText != "" {
		r.log.Error("claude: stderr tail",
			"err", terminal.Error,
			"stderr", stderrText,
		)
	}
	r.log.Info("claude: exited",
		"cost_usd", terminal.CostUSD,
		"duration_ms", terminal.DurationMS,
		"err", terminal.Error,
	)
	// Context-aware send of the terminal ResultEvent (issue #27). The consumer
	// (runTurn in internal/bot) stops draining `out` the instant the turn ctx is
	// cancelled (/stop or turn timeout). If ctx is already cancelled and at least
	// eventBuffer (32) events are still buffered, a bare `out <- terminal` would
	// block forever on the full channel against a dead consumer, leaking this
	// supervise goroutine (plus the retained channel and stderrTail) per
	// cancelled/timed-out turn. Selecting on ctx.Done() lets supervise exit and
	// the deferred close(out) fire even when nobody reads the final event.
	select {
	case out <- terminal:
	case <-ctx.Done():
	}
}

// closeWatchdog starts a goroutine that force-closes stdout if ctx finishes and
// the reader has not already finished within readGrace. It returns a stop func
// that supervise calls on the normal path (after parseStream reached EOF) to
// tear the watchdog down without ever touching stdout.
//
// Why this can't deadlock: the only way wg.Wait() in supervise blocks forever is
// a parseStream Read that never returns. That Read returns when (a) the pipe hits
// EOF — every write-end closed — or (b) the pipe's read end is closed. The
// watchdog guarantees (b): ctx (the bot's turnCtx) is ALWAYS cancelled (turn
// timeout or /stop), so the <-ctx.Done() arm always fires; readGrace later it
// calls stdout.Close(), which makes the blocked Read return an error promptly.
// supervise therefore returns within readGrace of ctx being done in the worst
// case, with no dependence on claude exiting or on os/exec's WaitDelay.
//
// Why the stdout.Close() vs parseStream Read race is safe: stdout is the read
// end of an os.Pipe (an *os.File). os.File.Close is safe for concurrent use and
// is idempotent — its internal poll.FD serializes Close against an in-flight
// Read and wakes the Read with a "file already closed" / EOF-ish error rather
// than corrupting state or panicking. parseStream treats any read error as
// end-of-stream (it returns the organic result and a possibly-non-nil err; on a
// cancelled ctx terminalResult discards parseErr in favour of ErrTimeout), so a
// force-close cannot manufacture a bogus success. The same fd is also closed by
// os/exec inside cmd.Wait() (and possibly by its WaitDelay path); a double/
// concurrent Close on an *os.File is harmless — one caller wins, the other gets
// ErrClosed — so the watchdog racing os/exec is fine. Crucially the watchdog is
// stopped (stopWatchdog) before cmd.Wait() on the normal path, so in the common
// case only os/exec ever closes the fd.
func (r *execRunner) closeWatchdog(ctx context.Context, stdout io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			// Normal path: parseStream reached EOF and supervise stopped us.
			return
		case <-ctx.Done():
		}
		// ctx is done. Give a well-behaved claude (SIGTERM'd via cmd.Cancel) a
		// brief grace to flush and close its stdout naturally — preserving the
		// final result frame on the clean-cancel path — then force the issue.
		select {
		case <-done:
			return
		case <-time.After(readGrace):
		}
		_ = stdout.Close()
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func (r *execRunner) terminalResult(
	ctx context.Context,
	organic *ResultEvent,
	waitErr error,
	parseErr error,
	stderrText string,
) ResultEvent {
	// Context cancellation wins over everything (timeout vs caller cancel).
	if ctxErr := ctx.Err(); ctxErr != nil {
		ev := ResultEvent{}
		if organic != nil {
			ev.CostUSD = organic.CostUSD
			ev.DurationMS = organic.DurationMS
		}
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			ev.Error = ErrTimeout
		} else {
			ev.Error = ctxErr
		}
		return ev
	}

	// No ctx cancel. If we parsed a clean result and the process exited 0, use it.
	if organic != nil && waitErr == nil && organic.Error == nil {
		return *organic
	}

	// Subprocess crashed or returned an error. Build a tagged error.
	ev := ResultEvent{}
	if organic != nil {
		ev.CostUSD = organic.CostUSD
		ev.DurationMS = organic.DurationMS
	}
	switch {
	case organic != nil && organic.Error != nil:
		ev.Error = classifyStderr(organic.Error, stderrText)
	case waitErr != nil:
		ev.Error = classifyStderr(fmt.Errorf("%w: %v", ErrCrash, waitErr), stderrText)
	case parseErr != nil:
		ev.Error = fmt.Errorf("%w: parse: %v", ErrCrash, parseErr)
	default:
		// Process exited 0 but emitted no result. Treat as crash.
		ev.Error = fmt.Errorf("%w: no result event", ErrCrash)
	}
	return ev
}

// classifyStderr only inspects stderr to pick a typed error; the stderr text
// itself never escapes into the returned error. Stderr may include API key
// fragments, account identifiers, or stack frames, and the returned error
// flows directly into a user-visible reply (see bot.errorSuffix). Raw stderr
// is logged separately at error level in supervise().
func classifyStderr(base error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "rate limit") || strings.Contains(low, "rate_limit"):
		return ErrRateLimit
	case strings.Contains(low, "authentication") || strings.Contains(low, "unauthorized") || strings.Contains(low, "api key"):
		return ErrAuth
	}
	return base
}

// shortSession returns the first 8 chars of a session id, matching the
// logging convention in DESIGN.md ("session id (first 8)"). Returns the
// original string when shorter than 8 chars (e.g. "" for fresh sessions).
func shortSession(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// tailBuffer keeps only the last `max` bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		excess := t.buf.Len() - t.max
		t.buf.Next(excess)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
