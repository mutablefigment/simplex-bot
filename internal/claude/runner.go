package claude

import (
	"context"
	"errors"
	"log/slog"

	"claude-bot/internal/config"
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

// execRunner is the skeleton stub. It satisfies the always-last-ResultEvent
// contract by emitting one ResultEvent with a "not implemented" error and
// closing the channel.
type execRunner struct {
	cfg config.Claude
	log *slog.Logger
}

var errNotImplemented = errors.New("claude.Runner: not implemented (skeleton stub)")

func (r *execRunner) Run(ctx context.Context, prompt, sessionID string) (<-chan Event, error) {
	out := make(chan Event, 1)
	r.log.Info("claude: would exec", "binary", r.cfg.Binary, "session", sessionID, "prompt_len", len(prompt))
	out <- ResultEvent{Error: errNotImplemented}
	close(out)
	return out, nil
}
