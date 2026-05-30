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

	var (
		organic *ResultEvent
		parseErr error
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		organic, parseErr = parseStream(ctx, stdout, out)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

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
