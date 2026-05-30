package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	version string
	simplex simplex.Client
	claude  claude.Runner
	store   store.Store

	queue chan job

	mu           sync.Mutex
	activeCancel context.CancelFunc // set while a turn is running; /stop calls it
}

// job is a unit of work for the worker. Exactly one of prompt or slash is set.
// /stop is NOT routed through here — it short-circuits in handleChatItem and
// cancels the active turn directly via activeCancel.
type job struct {
	prompt     string
	slash      *Cmd
	itemID     int64
	contactID  int64
	receivedAt time.Time
}

func New(
	cfg *config.Config,
	log *slog.Logger,
	version string,
	sx simplex.Client,
	cr claude.Runner,
	st store.Store,
) *Bot {
	return &Bot{
		cfg:     cfg,
		log:     log,
		version: version,
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

	go b.runInboxSweeper(ctx)

	workerDone := make(chan struct{})
	workerStarted := false

	for ev := range events {
		switch e := ev.(type) {
		case simplex.ConnectedEvent:
			b.log.Info("simplex: connected")
			if !workerStarted {
				// Startup cleanup talks to the wire (Finalise per orphan,
				// each up to ~15s on RPC timeout). Do it off-loop so the
				// event-dispatch loop keeps reading from the WS channel; if
				// the user sends a prompt before cleanup is done the worker
				// will pick it up — orphan finalisation can race the first
				// real turn without harm.
				go func() {
					if err := b.runStartupCleanup(ctx); err != nil {
						b.log.Warn("startup cleanup", "err", err)
					}
				}()
				go func() {
					defer close(workerDone)
					b.runWorker(ctx)
				}()
				workerStarted = true
			}
		case simplex.DisconnectedEvent:
			b.log.Warn("simplex: disconnected", "err", e.Err)
		case simplex.ChatItemsEvent:
			b.handleChatItem(ctx, e)
		}
	}
	if workerStarted {
		close(b.queue)
		<-workerDone
	}
	return ctx.Err()
}

// runStartupCleanup finalises any live messages left dangling by a previous
// process and marks any 'running' turns as cancelled. Called once after the
// first ConnectedEvent and before the worker starts pulling jobs, so a fresh
// inbound prompt can't interleave with the cleanup.
//
// Wire-side Finalise failures are non-fatal — we mark the DB row finalised
// anyway to avoid an unbounded loop on phantom items (e.g. the user deleted
// the chat). Original partial text is discarded: the live_messages mirror is
// the authoritative record of what we authored, so there's nothing to recover
// from the wire side.
func (b *Bot) runStartupCleanup(ctx context.Context) error {
	orphans, err := b.store.UnfinalisedLiveMessages(ctx)
	if err != nil {
		return fmt.Errorf("query orphans: %w", err)
	}
	for _, o := range orphans {
		if err := b.simplex.Finalise(ctx, o.ContactID, o.ItemID, "⚠️ bot restarted"); err != nil {
			b.log.Warn("orphan finalise: wire call failed; marking row finalised anyway",
				"item_id", o.ItemID, "err", err)
		}
		if err := b.store.FinaliseLiveMessage(ctx, o.ItemID); err != nil {
			b.log.Error("orphan finalise: db update failed", "item_id", o.ItemID, "err", err)
		}
	}
	if len(orphans) > 0 {
		b.log.Info("startup: finalised orphan live messages", "count", len(orphans))
	}
	if err := b.store.MarkStaleRunningTurns(ctx); err != nil {
		return fmt.Errorf("mark stale turns: %w", err)
	}
	return nil
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

	// /stop is handled out-of-queue: by the time it would dequeue, the turn
	// it's trying to cancel would already be over.
	if cmd, ok := parseCommand(text); ok && cmd.Name == "stop" {
		b.handleStop(ctx, ev)
		return
	}

	j := job{itemID: ev.ItemID, contactID: ev.ContactID, receivedAt: time.Now()}
	if cmd, ok := parseCommand(text); ok {
		j.slash = &cmd
	} else {
		j.prompt = text
	}

	select {
	case b.queue <- j:
		if j.slash != nil {
			b.log.Info("queued slash", "name", j.slash.Name)
		} else {
			b.log.Info("queued turn", "prompt_preview", preview(text, b.cfg.Log.LogFullMessages))
		}
	default:
		b.log.Warn("queue full; dropping message")
		_, _ = b.simplex.Send(ctx, ev.ContactID, "⚠️ busy — message dropped (queue full)", ev.ItemID)
	}
}

func (b *Bot) handleStop(ctx context.Context, ev simplex.ChatItemsEvent) {
	b.mu.Lock()
	cancel := b.activeCancel
	b.mu.Unlock()
	if cancel == nil {
		// Don't block the event loop on a WS RPC. Send takes the call
		// timeout (~10s) + write timeout (~5s) and the event dispatcher
		// is the only thing draining the WS read channel — backing it up
		// stalls further inbound messages.
		go func() {
			_, _ = b.simplex.Send(ctx, ev.ContactID, "nothing to stop", ev.ItemID)
		}()
		return
	}
	cancel()
	b.log.Info("/stop: turn cancelled")
}

func (b *Bot) runSlash(ctx context.Context, j job) {
	switch j.slash.Name {
	case "new":
		if err := b.store.ClearSession(ctx); err != nil {
			b.log.Error("clear session", "err", err)
			_, _ = b.simplex.Send(ctx, j.contactID, "⚠️ failed to clear session: "+err.Error(), j.itemID)
			return
		}
		b.log.Info("session cleared")
		_, _ = b.simplex.Send(ctx, j.contactID, "session cleared", j.itemID)
	case "help":
		_, _ = b.simplex.Send(ctx, j.contactID, helpText, j.itemID)
	case "status":
		_, _ = b.simplex.Send(ctx, j.contactID, b.statusText(ctx), j.itemID)
	case "cost":
		_, _ = b.simplex.Send(ctx, j.contactID, b.costText(ctx), j.itemID)
	default:
		_, _ = b.simplex.Send(ctx, j.contactID, "unknown command: /"+j.slash.Name, j.itemID)
	}
}

func (b *Bot) statusText(ctx context.Context) string {
	var lines []string
	if b.version != "" {
		lines = append(lines, "version: "+b.version)
	}
	sessionID, err := b.store.GetSessionID(ctx)
	if err != nil {
		b.log.Error("status: read session", "err", err)
	}
	if sessionID == "" {
		lines = append(lines, "session: (none — next prompt starts fresh)")
	} else {
		lines = append(lines, "session: "+shortSession(sessionID))
	}
	last, ok, err := b.store.LatestTurn(ctx)
	if err != nil {
		b.log.Error("status: read latest turn", "err", err)
	}
	if !ok {
		lines = append(lines, "last turn: (none)")
	} else {
		when := last.EndedAt
		if when.IsZero() {
			when = last.StartedAt
		}
		lines = append(lines, fmt.Sprintf("last turn: %s at %s",
			last.Status, when.UTC().Format("2006-01-02 15:04:05Z")))
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) costText(ctx context.Context) string {
	total, err := b.store.TotalCost(ctx)
	if err != nil {
		b.log.Error("cost: read total", "err", err)
		return "⚠️ cost lookup failed — check journal"
	}
	count, err := b.store.TurnCount(ctx)
	if err != nil {
		b.log.Error("cost: read turn count", "err", err)
		return "⚠️ cost lookup failed — check journal"
	}
	return fmt.Sprintf("$%.4f total over %d turns", total, count)
}

func shortSession(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

const helpText = `commands:
/new — start a fresh Claude session
/stop — cancel the current turn (if any)
/status — current session + last turn
/cost — total $ spent + turn count
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
			if j.slash != nil {
				b.runSlash(ctx, j)
				continue
			}
			b.runTurn(ctx, j)
		}
	}
}

func (b *Bot) runTurn(parent context.Context, j job) {
	turnCtx, cancel := context.WithTimeout(parent, time.Duration(b.cfg.Claude.TurnTimeout))
	b.mu.Lock()
	b.activeCancel = cancel
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.activeCancel = nil
		b.mu.Unlock()
		cancel()
	}()

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
		_, _ = b.simplex.Send(parent, j.contactID,
			fmt.Sprintf("⚠️ failed to start claude: %v", err), j.itemID)
		b.finishTurn(parent, turnID, turnRow, "error", 0, err)
		return
	}

	lt := newLiveTurn(b.log, b.simplex, b.store, j.contactID, j.itemID, b.cfg.LiveMessage.ChunkThreshold)

	interval := time.Duration(b.cfg.LiveMessage.UpdateInterval)
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var (
		terminal    claude.ResultEvent
		gotTerminal bool
	)
loop:
	for {
		select {
		case <-turnCtx.Done():
			break loop
		case <-ticker.C:
			if err := lt.Flush(turnCtx); err != nil {
				b.log.Warn("live flush", "err", err)
			}
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			switch e := ev.(type) {
			case claude.InitEvent:
				if e.SessionID != "" && e.SessionID != sessionID {
					if err := b.store.SetSessionID(turnCtx, e.SessionID); err != nil {
						b.log.Error("persist session id", "err", err)
					}
				}
			case claude.AssistantTextEvent:
				lt.Append(e.Text)
			case claude.ToolUseEvent:
				// suppressed in this slice; ShowToolUse honored later.
			case claude.ResultEvent:
				terminal = e
				gotTerminal = true
			}
		}
	}

	// Final pre-Finalise flush uses parent ctx (not turnCtx): claude may have
	// buffered the full response into a single trailing event, in which case
	// the loop exits with non-empty buf and we need to lazy-open + insert the
	// live_messages row even after /stop or timeout cancelled turnCtx.
	if err := lt.Flush(parent); err != nil {
		b.log.Debug("live flush final", "err", err)
	}

	suffix, status, runErr := b.classifyTurn(turnCtx, gotTerminal, terminal)

	sent, err := lt.Finalise(parent, suffix)
	if err != nil {
		b.log.Error("live finalise", "err", err)
	}
	if !sent {
		body := lt.FinaliseText()
		if strings.TrimSpace(body) == "" {
			body = "(empty response)"
		}
		if _, err := b.simplex.Send(parent, j.contactID, body, j.itemID); err != nil {
			b.log.Error("send fallback reply", "err", err)
		}
	}

	b.finishTurn(parent, turnID, turnRow, status, terminal.CostUSD, runErr)
	b.log.Info("turn end", "status", status, "cost_usd", terminal.CostUSD, "duration_ms", terminal.DurationMS)
}

// classifyTurn decides the suffix to attach to the final message, the status
// to record on the turn row, and the runErr to log/return. Cancellation by
// /stop or the turn timeout is detected via turnCtx.Err().
func (b *Bot) classifyTurn(turnCtx context.Context, gotTerminal bool, terminal claude.ResultEvent) (suffix, status string, runErr error) {
	switch {
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
		return "⏱️ timeout", "timeout", claude.ErrTimeout
	case errors.Is(turnCtx.Err(), context.Canceled):
		return "⚠️ interrupted", "cancelled", context.Canceled
	case !gotTerminal:
		err := fmt.Errorf("%w: runner closed channel without ResultEvent", claude.ErrCrash)
		return errorSuffix(err), "error", err
	case terminal.Error != nil:
		return errorSuffix(terminal.Error), errorStatus(terminal.Error), terminal.Error
	default:
		if b.cfg.Claude.ShowCostFooter && (terminal.CostUSD > 0 || terminal.DurationMS > 0) {
			return costSuffix(terminal.CostUSD, terminal.DurationMS), "ok", nil
		}
		return "", "ok", nil
	}
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
	// Detach: turn-row bookkeeping shouldn't be lost just because /stop or the
	// turn timeout cancelled the parent ctx.
	dctx, dcancel := detached()
	if err := b.store.UpdateTurn(dctx, base); err != nil {
		b.log.Error("update turn row", "err", err)
	}
	dcancel()
}

// errorSuffix returns a fixed, opaque suffix for the user-visible reply.
// Never include err.Error(): claude's stderr/errors may contain API key
// fragments or account identifiers (issue #4). Operators read journald.
func errorSuffix(err error) string {
	switch {
	case errors.Is(err, claude.ErrTimeout):
		return "⏱️ timeout"
	case errors.Is(err, claude.ErrRateLimit):
		return "🚦 rate limit — check journal"
	case errors.Is(err, claude.ErrAuth):
		return "🔑 auth error — check journal"
	case errors.Is(err, context.Canceled):
		return "⚠️ interrupted"
	default:
		return "⚠️ error — check journal"
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

func costSuffix(cost float64, durationMS int64) string {
	return fmt.Sprintf("— $%.4f · %.1fs", cost, float64(durationMS)/1000)
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
