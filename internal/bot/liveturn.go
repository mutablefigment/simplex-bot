package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"claude-bot/internal/store"
)

const defaultChunkThreshold = 4096

// liveSender is the subset of simplex.Client the LiveTurn FSM needs. It exists
// so unit tests can drive the FSM with a tiny fake instead of standing up the
// whole simplex client.
type liveSender interface {
	SendLive(ctx context.Context, contactID int64, text string, quotedItemID int64) (int64, error)
	UpdateLive(ctx context.Context, contactID, itemID int64, text string) error
	Finalise(ctx context.Context, contactID, itemID int64, text string) error
}

// LiveTurn streams a single assistant reply as a SimpleX live message.
//
// Driven sequentially by runTurn (no internal goroutine, no mutex):
//   - Append on each assistant text event.
//   - Flush on every 3s tick (and once after the event stream closes via
//     Finalise's pre-flush).
//   - Finalise once at the end with an optional suffix (cost footer or error).
//
// Lazy-open: the first SendLive isn't issued until the first non-empty Flush,
// so a turn that produces zero assistant text never opens a live message.
//
// Rotation: when a flushed message exceeds chunkThreshold the FSM finalises
// the current live item and resets so the next Append opens a fresh one (no
// quote on follow-ups; only the first message of a turn quotes the prompt).
type LiveTurn struct {
	log       *slog.Logger
	sender    liveSender
	store     store.Store
	contactID int64
	promptID  int64

	chunkThreshold int

	buf              strings.Builder
	itemID           int64  // 0 = not currently open
	quoted           bool   // true after the first SendLive of the turn
	flushed          string // last translated text actually sent on the wire
	lastFinaliseText string // composed text from the last Finalise call
}

func newLiveTurn(
	log *slog.Logger,
	sender liveSender,
	st store.Store,
	contactID, promptID int64,
	chunkThreshold int,
) *LiveTurn {
	if chunkThreshold <= 0 {
		chunkThreshold = defaultChunkThreshold
	}
	return &LiveTurn{
		log:            log,
		sender:         sender,
		store:          st,
		contactID:      contactID,
		promptID:       promptID,
		chunkThreshold: chunkThreshold,
	}
}

// Append accumulates assistant text. Does not touch the wire.
func (lt *LiveTurn) Append(text string) {
	lt.buf.WriteString(text)
}

// Flush translates the cumulative buffer and pushes it as a live update.
// On first non-empty call it lazy-opens the live message via SendLive
// (quoting the user prompt). On subsequent calls it sends UpdateLive with the
// full cumulative text. No-op when the buffer is empty or unchanged.
//
// If the translated cumulative exceeds chunkThreshold, the current live item
// is finalised (no live=on) and the buffer is reset so the next Append starts
// fresh on a new live item.
func (lt *LiveTurn) Flush(ctx context.Context) error {
	text := translateMarkdown(lt.buf.String())
	if text == "" || text == lt.flushed {
		return nil
	}

	if lt.itemID == 0 {
		var qid int64
		if !lt.quoted {
			qid = lt.promptID
		}
		id, err := lt.sender.SendLive(ctx, lt.contactID, text, qid)
		if err != nil {
			return fmt.Errorf("simplex SendLive: %w", err)
		}
		lt.itemID = id
		lt.quoted = true
		// Detach from the caller's ctx: local sqlite writes are bookkeeping,
		// not subject to user cancellation. If we used `ctx` and a tick
		// happened to race against /stop or timeout, the row wouldn't get
		// written and orphan cleanup on next restart would miss the live
		// message.
		if err := lt.store.InsertLiveMessage(detached(ctx), store.LiveMessage{
			ItemID:    id,
			ContactID: lt.contactID,
			StartedAt: time.Now().UTC(),
		}); err != nil {
			lt.log.Error("insert live_message", "err", err)
		}
	} else {
		if err := lt.sender.UpdateLive(ctx, lt.contactID, lt.itemID, text); err != nil {
			return fmt.Errorf("simplex UpdateLive: %w", err)
		}
	}
	lt.flushed = text

	if len(text) > lt.chunkThreshold {
		return lt.rotate(ctx)
	}
	return nil
}

// rotate finalises the current live message and resets the buffer so the next
// Append opens a fresh live message. Called internally from Flush when the
// cumulative crosses chunkThreshold.
func (lt *LiveTurn) rotate(ctx context.Context) error {
	if err := lt.sender.Finalise(ctx, lt.contactID, lt.itemID, lt.flushed); err != nil {
		return fmt.Errorf("simplex Finalise (rotate): %w", err)
	}
	if err := lt.store.FinaliseLiveMessage(detached(ctx), lt.itemID); err != nil {
		lt.log.Error("mark live_message finalised", "err", err)
	}
	lt.itemID = 0
	lt.buf.Reset()
	lt.flushed = ""
	return nil
}

// Finalise composes the translated cumulative text with an optional suffix
// (cost footer or error tag — must be markdown-free), and closes the current
// live message without live=on. Returns sent=false when no live message was
// ever opened (empty turn or rotated-then-no-more-text); the caller can read
// FinaliseText() and fall back to a plain Send.
//
// The caller passes a context independent of the (possibly cancelled) turn ctx
// so finalisation goes through even on /stop or timeout.
func (lt *LiveTurn) Finalise(ctx context.Context, suffix string) (sent bool, err error) {
	text := translateMarkdown(lt.buf.String())
	if suffix != "" {
		if text != "" {
			text += "\n\n"
		}
		text += suffix
	}
	lt.lastFinaliseText = text

	if lt.itemID == 0 {
		return false, nil
	}
	if err := lt.sender.Finalise(ctx, lt.contactID, lt.itemID, text); err != nil {
		return false, fmt.Errorf("simplex Finalise: %w", err)
	}
	if err := lt.store.FinaliseLiveMessage(detached(ctx), lt.itemID); err != nil {
		lt.log.Error("mark live_message finalised", "err", err)
	}
	lt.itemID = 0
	lt.flushed = text
	return true, nil
}

// FinaliseText returns the composed text from the last Finalise call. Used by
// the empty-turn fallback path: when Finalise returns sent=false, the caller
// reads this and issues a plain Send.
func (lt *LiveTurn) FinaliseText() string {
	return lt.lastFinaliseText
}

// detached returns a background-derived ctx with a 5s timeout, used for local
// sqlite bookkeeping that shouldn't be cancelled by a user /stop or turn
// timeout. Returns parent ctx if it already has a Deadline (so tests with
// short timeouts still bound DB calls).
func detached(_ context.Context) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	return ctx
}
