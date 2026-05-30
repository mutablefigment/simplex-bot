package simplex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"claude-bot/internal/config"
)

const (
	readLimitBytes = 1 << 22
	callTimeout    = 10 * time.Second
	writeTimeout   = 5 * time.Second
	reconnectMin   = 1 * time.Second
	reconnectMax   = 30 * time.Second
	eventBuffer    = 64
)

type wsClient struct {
	cfg config.Simplex
	log *slog.Logger

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan responseFrame

	corrCtr atomic.Int64
	closed  atomic.Bool
	done    chan struct{}
}

type wsEnvelope struct {
	CorrID string          `json:"corrId,omitempty"`
	Cmd    string          `json:"cmd,omitempty"`
	Resp   json.RawMessage `json:"resp,omitempty"`
}

type respHeader struct {
	Type      string          `json:"type"`
	ChatError json.RawMessage `json:"chatError,omitempty"`
}

type responseFrame struct {
	typ string
	raw json.RawMessage
}

type chatItemBlock struct {
	ChatInfo struct {
		Contact struct {
			ContactID        int64  `json:"contactId"`
			LocalDisplayName string `json:"localDisplayName"`
		} `json:"contact"`
	} `json:"chatInfo"`
	ChatItem struct {
		ChatDir struct {
			Type string `json:"type"`
		} `json:"chatDir"`
		Content struct {
			MsgContent struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"msgContent"`
		} `json:"content"`
		Meta struct {
			ItemID   int64 `json:"itemId"`
			ItemLive bool  `json:"itemLive"`
		} `json:"meta"`
		QuotedItem *struct {
			ItemID int64 `json:"itemId"`
		} `json:"quotedItem,omitempty"`
	} `json:"chatItem"`
}

func (c *wsClient) Run(ctx context.Context) (<-chan Event, error) {
	c.pending = make(map[string]chan responseFrame)
	c.done = make(chan struct{})
	out := make(chan Event, eventBuffer)

	go c.connectLoop(ctx, out)
	return out, nil
}

func (c *wsClient) connectLoop(ctx context.Context, out chan<- Event) {
	defer close(out)
	defer close(c.done)

	backoff := reconnectMin
	for {
		if ctx.Err() != nil {
			return
		}

		conn, _, err := websocket.Dial(ctx, c.cfg.WSURL, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("simplex: dial failed", "url", c.cfg.WSURL, "err", err, "retry_in", backoff)
			emit(ctx, out, DisconnectedEvent{Err: err})
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		conn.SetReadLimit(readLimitBytes)

		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		c.log.Info("simplex: connected", "url", c.cfg.WSURL)
		emit(ctx, out, ConnectedEvent{})
		backoff = reconnectMin

		readErr := c.readLoop(ctx, conn, out)

		c.mu.Lock()
		c.conn = nil
		// fail any in-flight callers; they'll see this as a transport error.
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")

		if c.closed.Load() {
			return
		}
		c.log.Warn("simplex: disconnected", "err", readErr, "retry_in", backoff)
		emit(ctx, out, DisconnectedEvent{Err: readErr})
		if !sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (c *wsClient) readLoop(ctx context.Context, conn *websocket.Conn, out chan<- Event) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var env wsEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.log.Warn("simplex: malformed frame", "err", err)
			continue
		}
		if len(env.Resp) == 0 {
			continue
		}

		var hdr respHeader
		if err := json.Unmarshal(env.Resp, &hdr); err != nil {
			c.log.Warn("simplex: malformed resp", "err", err)
			continue
		}

		if env.CorrID != "" {
			c.deliverResponse(env.CorrID, responseFrame{typ: hdr.Type, raw: env.Resp})
			continue
		}
		c.handlePush(ctx, hdr.Type, env.Resp, out)
	}
}

func (c *wsClient) deliverResponse(corrID string, frame responseFrame) {
	c.mu.Lock()
	ch, ok := c.pending[corrID]
	if ok {
		delete(c.pending, corrID)
	}
	c.mu.Unlock()
	if ok {
		ch <- frame
		close(ch)
	}
}

func (c *wsClient) handlePush(ctx context.Context, typ string, raw json.RawMessage, out chan<- Event) {
	switch typ {
	case "newChatItems":
		var resp struct {
			ChatItems []chatItemBlock `json:"chatItems"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			c.log.Warn("simplex: decode newChatItems", "err", err)
			return
		}
		for _, it := range resp.ChatItems {
			if it.ChatItem.ChatDir.Type != "directRcv" {
				continue
			}
			ev := ChatItemsEvent{
				ContactID: it.ChatInfo.Contact.ContactID,
				ItemID:    it.ChatItem.Meta.ItemID,
				Text:      it.ChatItem.Content.MsgContent.Text,
			}
			if it.ChatItem.QuotedItem != nil {
				ev.QuotedItemID = it.ChatItem.QuotedItem.ItemID
			}
			emit(ctx, out, ev)
		}
	case "chatItemsStatusesUpdated", "chatItemUpdated",
		"contactSubSummary", "userContactSubSummary", "memberSubSummary",
		"pendingSubSummary", "terminalEvent":
		// known but ignored; chatItemUpdated includes echoes of our own live updates.
		c.log.Debug("simplex: push event ignored", "type", typ)
	default:
		c.log.Debug("simplex: unhandled push event", "type", typ)
	}
}

func (c *wsClient) call(ctx context.Context, cmd string) (responseFrame, error) {
	if c.closed.Load() {
		return responseFrame{}, errors.New("simplex: client closed")
	}

	id := strconv.FormatInt(c.corrCtr.Add(1), 10)
	ch := make(chan responseFrame, 1)

	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return responseFrame{}, errors.New("simplex: not connected")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(wsEnvelope{CorrID: id, Cmd: cmd})
	if err != nil {
		c.removePending(id)
		return responseFrame{}, fmt.Errorf("simplex: marshal envelope: %w", err)
	}

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	werr := conn.Write(wctx, websocket.MessageText, payload)
	cancel()
	if werr != nil {
		c.removePending(id)
		return responseFrame{}, fmt.Errorf("simplex: write: %w", werr)
	}

	rctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	select {
	case frame, ok := <-ch:
		if !ok {
			return responseFrame{}, errors.New("simplex: connection lost before response")
		}
		if frame.typ == "chatCmdError" {
			return frame, parseCmdError(frame.raw)
		}
		return frame, nil
	case <-rctx.Done():
		c.removePending(id)
		return responseFrame{}, fmt.Errorf("simplex: call timeout: %w", rctx.Err())
	}
}

func (c *wsClient) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func parseCmdError(raw json.RawMessage) error {
	var resp struct {
		ChatError struct {
			ErrorType struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"errorType"`
		} `json:"chatError"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("simplex: chatCmdError (undecodable): %w", err)
	}
	return fmt.Errorf("simplex: chatCmdError %s: %s", resp.ChatError.ErrorType.Type, resp.ChatError.ErrorType.Message)
}

func (c *wsClient) Send(ctx context.Context, contactID int64, text string, quotedItemID int64) (int64, error) {
	return c.sendCommon(ctx, buildSendCmd(contactID, text, quotedItemID, false))
}

func (c *wsClient) SendLive(ctx context.Context, contactID int64, text string, quotedItemID int64) (int64, error) {
	return c.sendCommon(ctx, buildSendCmd(contactID, text, quotedItemID, true))
}

func (c *wsClient) sendCommon(ctx context.Context, cmd string) (int64, error) {
	frame, err := c.call(ctx, cmd)
	if err != nil {
		return 0, err
	}
	if frame.typ != "newChatItems" {
		return 0, fmt.Errorf("simplex: unexpected response type %q for send", frame.typ)
	}
	var resp struct {
		ChatItems []chatItemBlock `json:"chatItems"`
	}
	if err := json.Unmarshal(frame.raw, &resp); err != nil {
		return 0, fmt.Errorf("simplex: decode send response: %w", err)
	}
	if len(resp.ChatItems) == 0 {
		return 0, errors.New("simplex: send response had no chatItems")
	}
	return resp.ChatItems[0].ChatItem.Meta.ItemID, nil
}

// buildSendCmd always emits the json composed-message form. The plain
// `/_send @<cid> [live=on] text <body>` form would concatenate `text` directly
// into the command string; any newline, CR, or other control byte in the body
// terminates the command early at simplex-chat's parser and lets the remainder
// be re-interpreted as a fresh CLI command. JSON-encoded bodies escape control
// characters, so embedded newlines stay inside the msgContent field.
func buildSendCmd(contactID int64, text string, quotedItemID int64, live bool) string {
	liveFlag := ""
	if live {
		liveFlag = " live=on"
	}
	msg := map[string]any{
		"msgContent": map[string]any{"type": "text", "text": text},
	}
	if quotedItemID != 0 {
		msg["quotedItemId"] = quotedItemID
	}
	payload, _ := json.Marshal([]map[string]any{msg})
	return fmt.Sprintf("/_send @%d%s json %s", contactID, liveFlag, payload)
}

// buildUpdateCmd emits `/_update item @<cid> <itemId> [live=on] json {...}` for
// the same reason as buildSendCmd: a `text <body>` suffix concatenates the body
// into the command string and is vulnerable to newline injection.
func buildUpdateCmd(contactID, itemID int64, text string, live bool) string {
	liveFlag := ""
	if live {
		liveFlag = " live=on"
	}
	payload, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	return fmt.Sprintf("/_update item @%d %d%s json %s", contactID, itemID, liveFlag, payload)
}

func (c *wsClient) UpdateLive(ctx context.Context, contactID, itemID int64, text string) error {
	return c.expectUpdate(ctx, buildUpdateCmd(contactID, itemID, text, true))
}

func (c *wsClient) Finalise(ctx context.Context, contactID, itemID int64, text string) error {
	return c.expectUpdate(ctx, buildUpdateCmd(contactID, itemID, text, false))
}

func (c *wsClient) expectUpdate(ctx context.Context, cmd string) error {
	frame, err := c.call(ctx, cmd)
	if err != nil {
		return err
	}
	if frame.typ != "chatItemUpdated" {
		return fmt.Errorf("simplex: unexpected response type %q for update", frame.typ)
	}
	return nil
}

// GetChats is used at startup to find live messages we own and finalise them.
// TODO(milestone-2): the exact command form for "all chats with their live items"
// hasn't been verified against simplex-chat. Implement once probed.
func (c *wsClient) GetChats(ctx context.Context) ([]Chat, error) {
	c.log.Debug("simplex.GetChats: not yet implemented")
	return nil, nil
}

func (c *wsClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}
	if c.done != nil {
		<-c.done
	}
	return nil
}

func emit(ctx context.Context, out chan<- Event, ev Event) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMax {
		d = reconnectMax
	}
	return d
}
