package bot

import (
	"context"
	"fmt"
	"log/slog"

	"claude-bot/internal/claude"
	"claude-bot/internal/config"
	"claude-bot/internal/simplex"
	"claude-bot/internal/store"
)

type Bot struct {
	cfg     *config.Config
	log     *slog.Logger
	simplex simplex.Client
	claude  claude.Runner // TODO(milestone-2): invoke from worker goroutine
	store   store.Store
}

func New(
	cfg *config.Config,
	log *slog.Logger,
	sx simplex.Client,
	cr claude.Runner,
	st store.Store,
) *Bot {
	return &Bot{cfg: cfg, log: log, simplex: sx, claude: cr, store: st}
}

func (b *Bot) Run(ctx context.Context) error {
	events, err := b.simplex.Run(ctx)
	if err != nil {
		return fmt.Errorf("simplex run: %w", err)
	}
	for ev := range events {
		b.log.Info("simplex event", "type", fmt.Sprintf("%T", ev))
	}
	return ctx.Err()
}
