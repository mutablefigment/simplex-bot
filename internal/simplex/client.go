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

	Send(ctx context.Context, contactID int64, text string, quotedItemID int64) (itemID int64, err error)
	SendLive(ctx context.Context, contactID int64, text string, quotedItemID int64) (itemID int64, err error)
	UpdateLive(ctx context.Context, contactID, itemID int64, text string) error
	Finalise(ctx context.Context, contactID, itemID int64, text string) error
	// ReceiveFile downloads an offered inbound attachment to destPath, blocking
	// until the transfer completes. Returns the path the bytes were written to.
	ReceiveFile(ctx context.Context, fileID int64, destPath string) (string, error)
	Close() error
}

// NOTE: there is intentionally no wire-side GetChats. Startup orphan cleanup
// uses the local sqlite live_messages mirror only — see bot.runStartupCleanup.
// simplex-chat v6.4.10 exposes /_get chat @<cid> count=N per-contact, but the
// bot only ever talks to one whitelisted contact and tracks every live message
// it opens, so the local mirror is authoritative for what we authored.
// Re-introducing a wire probe would be cosmetic (e.g. to recover partial text
// from items the DB lost) and is not currently planned.

func New(cfg config.Simplex, log *slog.Logger) Client {
	return &wsClient{cfg: cfg, log: log}
}
