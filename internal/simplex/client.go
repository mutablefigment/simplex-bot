package simplex

import (
	"context"
	"log/slog"

	"claude-bot/internal/config"
)

type Client interface {
	// Run dials the WS endpoint and starts the read+reconnect loop.
	// The returned channel emits events until ctx is done; it is closed
	// after the connection has been torn down.
	Run(ctx context.Context) (<-chan Event, error)

	SendLive(ctx context.Context, contactID int64, text string, quotedItemID int64) (itemID int64, err error)
	UpdateLive(ctx context.Context, contactID, itemID int64, text string) error
	Finalise(ctx context.Context, contactID, itemID int64, text string) error
	GetChats(ctx context.Context) ([]Chat, error)
	Close() error
}

func New(cfg config.Simplex, log *slog.Logger) Client {
	return &wsClient{cfg: cfg, log: log}
}
