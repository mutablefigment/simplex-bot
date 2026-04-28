package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"claude-bot/internal/claude"
	"claude-bot/internal/config"
	"claude-bot/internal/simplex"
	"claude-bot/internal/store"
)

const queueDepth = 16

type Bot struct {
	cfg     *config.Config
	log     *slog.Logger
	simplex simplex.Client
	claude  claude.Runner
	store   store.Store

	queue chan job
}

type job struct {
	prompt   string
	itemID   int64 // user's message itemID for quoting (unused in this slice)
	receivedAt time.Time
}

func New(
	cfg *config.Config,
	log *slog.Logger,
	sx simplex.Client,
	cr claude.Runner,
	st store.Store,
) *Bot {
	return &Bot{
		cfg:     cfg,
		log:     log,
		simplex: sx,
		claude:  cr,
		store:   st,
		queue:   make(chan job, queueDepth),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	events, err := b.simplex.Run(ctx)
	if err != nil {
		return fmt.Errorf("simplex run: %w", err)
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		b.runWorker(ctx)
	}()

	for ev := range events {
		b.handleEvent(ctx, ev)
	}
	close(b.queue)
	<-workerDone
	return ctx.Err()
}

func (b *Bot) handleEvent(ctx context.Context, ev simplex.Event) {
	switch e := ev.(type) {
	case simplex.ConnectedEvent:
		b.log.Info("simplex: connected")
	case simplex.DisconnectedEvent:
		b.log.Warn("simplex: disconnected", "err", e.Err)
	case simplex.ChatItemsEvent:
		b.handleChatItem(ctx, e)
	}
}

func (b *Bot) handleChatItem(ctx context.Context, ev simplex.ChatItemsEvent) {
	if ev.ContactID != b.cfg.Simplex.AllowedContactID {
		b.log.Warn("rejected message from non-whitelisted contact",
			"contact_id", ev.ContactID, "allowed", b.cfg.Simplex.AllowedContactID)
		return
	}

	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}

	if cmd, ok := parseCommand(text); ok {
		b.handleSlash(ctx, ev, cmd)
		return
	}

	select {
	case b.queue <- job{prompt: text, itemID: ev.ItemID, receivedAt: time.Now()}:
		b.log.Info("queued turn", "prompt_preview", preview(text, b.cfg.Log.LogFullMessages))
	default:
		b.log.Warn("queue full; dropping turn")
		_, _ = b.simplex.Send(ctx, ev.ContactID, "⚠️ busy — message dropped (queue full)", ev.ItemID)
	}
}

func (b *Bot) handleSlash(ctx context.Context, ev simplex.ChatItemsEvent, cmd Cmd) {
	switch cmd.Name {
	case "new":
		if err := b.store.ClearSession(ctx); err != nil {
			b.log.Error("clear session", "err", err)
			_, _ = b.simplex.Send(ctx, ev.ContactID, "⚠️ failed to clear session: "+err.Error(), ev.ItemID)
			return
		}
		_, _ = b.simplex.Send(ctx, ev.ContactID, "session cleared", ev.ItemID)
	case "help":
		_, _ = b.simplex.Send(ctx, ev.ContactID, helpText, ev.ItemID)
	default:
		_, _ = b.simplex.Send(ctx, ev.ContactID, "unknown command: /"+cmd.Name, ev.ItemID)
	}
}

const helpText = `commands:
/new — start a fresh Claude session
/help — show this message
anything else is sent to Claude as a prompt`

func (b *Bot) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-b.queue:
			if !ok {
				return
			}
			b.runTurn(ctx, j)
		}
	}
}

func (b *Bot) runTurn(parent context.Context, j job) {
	turnCtx, cancel := context.WithTimeout(parent, time.Duration(b.cfg.Claude.TurnTimeout))
	defer cancel()

	sessionID, err := b.store.GetSessionID(turnCtx)
	if err != nil {
		b.log.Error("load session", "err", err)
		sessionID = ""
	}

	turnRow := store.Turn{
		SessionID: sessionID,
		StartedAt: time.Now().UTC(),
		Status:    "running",
	}
	turnID, err := b.store.InsertTurn(turnCtx, turnRow)
	if err != nil {
		b.log.Error("insert turn row", "err", err)
	}

	b.log.Info("turn start",
		"prompt_preview", preview(j.prompt, b.cfg.Log.LogFullMessages),
		"resume", sessionID != "",
	)

	events, err := b.claude.Run(turnCtx, j.prompt, sessionID)
	if err != nil {
		b.log.Error("claude run", "err", err)
		_, _ = b.simplex.Send(parent, b.cfg.Simplex.AllowedContactID,
			fmt.Sprintf("⚠️ failed to start claude: %v", err), j.itemID)
		b.finishTurn(parent, turnID, turnRow, "error", 0, err)
		return
	}

	var (
		buf            strings.Builder
		newSessionID   string
		terminal       claude.ResultEvent
		gotTerminal    bool
	)
	for ev := range events {
		switch e := ev.(type) {
		case claude.InitEvent:
			newSessionID = e.SessionID
			if newSessionID != "" && newSessionID != sessionID {
				if err := b.store.SetSessionID(turnCtx, newSessionID); err != nil {
					b.log.Error("persist session id", "err", err)
				}
			}
		case claude.AssistantTextEvent:
			buf.WriteString(e.Text)
		case claude.ToolUseEvent:
			// suppressed in slice; b.cfg.Claude.ShowToolUse honored in next milestone.
		case claude.ResultEvent:
			terminal = e
			gotTerminal = true
		}
	}

	if !gotTerminal {
		// Defensive — runner contract guarantees a terminal ResultEvent. If it
		// breaks, surface as a crash rather than dropping the turn.
		terminal = claude.ResultEvent{Error: fmt.Errorf("%w: runner closed channel without ResultEvent", claude.ErrCrash)}
	}

	body := buf.String()
	status := "ok"
	switch {
	case terminal.Error != nil:
		body = appendErrorTag(body, terminal.Error)
		status = errorStatus(terminal.Error)
	case b.cfg.Claude.ShowCostFooter:
		body = appendCostFooter(body, terminal.CostUSD, terminal.DurationMS)
	}

	if strings.TrimSpace(body) == "" {
		body = "(empty response)"
	}

	if _, err := b.simplex.Send(parent, b.cfg.Simplex.AllowedContactID, body, j.itemID); err != nil {
		b.log.Error("send reply", "err", err)
	}

	b.finishTurn(parent, turnID, turnRow, status, terminal.CostUSD, terminal.Error)
	b.log.Info("turn end", "status", status, "cost_usd", terminal.CostUSD, "duration_ms", terminal.DurationMS)
}

func (b *Bot) finishTurn(ctx context.Context, turnID int64, base store.Turn, status string, cost float64, runErr error) {
	if turnID == 0 {
		return
	}
	base.ID = turnID
	base.EndedAt = time.Now().UTC()
	base.Status = status
	base.CostUSD = cost
	if runErr != nil {
		base.Error = runErr.Error()
	}
	if err := b.store.UpdateTurn(ctx, base); err != nil {
		b.log.Error("update turn row", "err", err)
	}
}

func appendErrorTag(body string, err error) string {
	tag := errorTag(err)
	if body == "" {
		return tag
	}
	return body + "\n\n" + tag
}

func errorTag(err error) string {
	switch {
	case errors.Is(err, claude.ErrTimeout):
		return "⏱️ timeout"
	case errors.Is(err, claude.ErrRateLimit):
		return "🚦 rate limit: " + err.Error()
	case errors.Is(err, claude.ErrAuth):
		return "🔑 auth error: " + err.Error()
	case errors.Is(err, context.Canceled):
		return "⚠️ interrupted"
	default:
		return "⚠️ error: " + err.Error()
	}
}

func errorStatus(err error) string {
	switch {
	case errors.Is(err, claude.ErrTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}

func appendCostFooter(body string, cost float64, durationMS int64) string {
	if cost == 0 && durationMS == 0 {
		return body
	}
	footer := fmt.Sprintf("— $%.4f · %.1fs", cost, float64(durationMS)/1000)
	if body == "" {
		return footer
	}
	return body + "\n\n" + footer
}

func preview(s string, full bool) string {
	if full {
		return s
	}
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
