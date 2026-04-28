package simplex

import (
	"context"
	"log/slog"

	"claude-bot/internal/config"
)

// wsClient is the skeleton stub. It logs intent on Run, emits no events,
// and closes the channel when ctx is done. Real dialing/reconnect comes
// in milestone 2.
type wsClient struct {
	cfg config.Simplex
	log *slog.Logger
}

func (c *wsClient) Run(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event)
	c.log.Info("simplex: would dial", "ws_url", c.cfg.WSURL)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}

func (c *wsClient) SendLive(ctx context.Context, contactID int64, text string, quotedItemID int64) (int64, error) {
	c.log.Debug("simplex.SendLive stub", "contact", contactID, "quoted", quotedItemID)
	return 0, nil
}

func (c *wsClient) UpdateLive(ctx context.Context, contactID, itemID int64, text string) error {
	c.log.Debug("simplex.UpdateLive stub", "contact", contactID, "item", itemID)
	return nil
}

func (c *wsClient) Finalise(ctx context.Context, contactID, itemID int64, text string) error {
	c.log.Debug("simplex.Finalise stub", "contact", contactID, "item", itemID)
	return nil
}

func (c *wsClient) GetChats(ctx context.Context) ([]Chat, error) {
	return nil, nil
}

func (c *wsClient) Close() error {
	return nil
}
