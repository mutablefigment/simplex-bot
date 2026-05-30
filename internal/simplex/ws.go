package simplex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

	mu          sync.Mutex
	conn        *websocket.Conn
	pending     map[string]chan responseFrame
	fileWaiters map[int64]chan error // fileId -> completion signal for ReceiveFile
	// abandoned tracks destPaths for transfers ReceiveFile gave up on (timeout/
	// cancel) but which simplex-chat may still finish writing. A late, otherwise
	// unrouted rcvFileComplete/rcvFileError for such a fileId triggers cleanup of
	// the orphaned file. See issue #34.
	abandoned map[int64]string

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
		File *ciFileBlock `json:"file,omitempty"`
	} `json:"chatItem"`
}

// ciFileBlock mirrors simplex-chat's CIFile: the attachment metadata that hangs
// off a chat item alongside its msgContent.
type ciFileBlock struct {
	FileID     int64  `json:"fileId"`
	FileName   string `json:"fileName"`
	FileSize   int64  `json:"fileSize"`
	FileStatus struct {
		Type string `json:"type"`
	} `json:"fileStatus"`
}

func (c *wsClient) Run(ctx context.Context) (<-chan Event, error) {
	c.pending = make(map[string]chan responseFrame)
	c.fileWaiters = make(map[int64]chan error)
	c.abandoned = make(map[int64]string)
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
		for id, ch := range c.fileWaiters {
			ch <- errors.New("simplex: connection lost during file transfer")
			close(ch)
			delete(c.fileWaiters, id)
		}
		// Drop any abandoned-transfer bookkeeping: simplex-chat's fileIds are
		// session-scoped, so a late completion will never arrive on a new
		// connection. The orphan is left for the age sweeper as a last resort.
		clear(c.abandoned)
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
			// An inbound attachment arrives as a sibling `file` block that still
			// needs pulling down (status rcvInvitation). The bot picks the
			// destination and calls ReceiveFile.
			if f := it.ChatItem.File; f != nil && f.FileID != 0 && f.FileStatus.Type == "rcvInvitation" {
				ev.Files = append(ev.Files, File{ID: f.FileID, Name: f.FileName, Size: f.FileSize})
			}
			emit(ctx, out, ev)
		}
	case "rcvFileComplete":
		c.deliverFile(raw, nil)
	case "rcvFileError", "rcvFileSndCancelled":
		c.deliverFile(raw, fmt.Errorf("simplex: file transfer %s", typ))
	case "rcvFileStart", "rcvFileAccepted", "rcvFileDescrReady":
		// progress events for an in-flight transfer; completion is signalled
		// by rcvFileComplete/rcvFileError, which is what ReceiveFile waits on.
		c.log.Debug("simplex: file transfer progress", "type", typ)
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

// ReceiveFile pulls an offered inbound file (by fileId) down to destPath and
// blocks until the transfer completes. It sends /freceive, confirms the
// rcvFileAccepted response, then waits for the rcvFileComplete push so the
// caller knows the bytes are fully on disk before reading them. On success it
// returns destPath (the location we asked simplex-chat to write to).
func (c *wsClient) ReceiveFile(ctx context.Context, fileID int64, destPath string) (string, error) {
	ch := make(chan error, 1)
	c.mu.Lock()
	if c.fileWaiters == nil {
		c.fileWaiters = make(map[int64]chan error)
	}
	c.fileWaiters[fileID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.fileWaiters[fileID] == ch {
			delete(c.fileWaiters, fileID)
		}
		c.mu.Unlock()
	}()

	frame, err := c.call(ctx, buildReceiveCmd(fileID, destPath))
	if err != nil {
		return "", err
	}
	switch frame.typ {
	case "rcvFileAccepted":
		// transfer started; fall through to await completion.
	case "rcvFileAcceptedSndCancelled":
		return "", fmt.Errorf("simplex: sender cancelled file %d", fileID)
	default:
		return "", fmt.Errorf("simplex: unexpected response %q to /freceive", frame.typ)
	}

	select {
	case e := <-ch:
		if e != nil {
			// Transfer error / connection lost: simplex-chat may have written a
			// partial file at destPath. The waiter is already gone (deliverFile
			// or connectLoop deleted it), so no late completion will arrive for
			// this fileId; remove the orphan now.
			c.removeReceivedFile(fileID, destPath)
			return "", e
		}
		return destPath, nil
	case <-ctx.Done():
		// Timeout/cancel: we are abandoning the transfer. simplex-chat keeps
		// running and may finish writing destPath at any point, including after
		// our os.Remove below (the post-timeout write race). Register destPath as
		// abandoned first so a late, otherwise-unrouted rcvFileComplete cleans up
		// whatever lands there, then best-effort remove anything already written.
		c.markAbandoned(fileID, destPath)
		c.removeReceivedFile(fileID, destPath)
		return "", ctx.Err()
	}
}

// markAbandoned records destPath so a late rcvFileComplete/rcvFileError for
// fileId (one that arrives after ReceiveFile gave up and removed its waiter)
// can remove the file simplex-chat finished writing post-timeout.
func (c *wsClient) markAbandoned(fileID int64, destPath string) {
	if destPath == "" {
		return
	}
	c.mu.Lock()
	if c.abandoned == nil {
		c.abandoned = make(map[int64]string)
	}
	c.abandoned[fileID] = destPath
	c.mu.Unlock()
}

// removeReceivedFile best-effort deletes a possibly-partial file simplex-chat
// wrote for an abandoned/failed transfer. A missing file is not an error (it may
// never have been created, or a late completion may have already cleaned it up).
func (c *wsClient) removeReceivedFile(fileID int64, destPath string) {
	if destPath == "" {
		return
	}
	if err := os.Remove(destPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.log.Warn("simplex: remove orphaned inbox file", "file_id", fileID, "path", destPath, "err", err)
	}
}

// buildReceiveCmd renders /freceive. destPath, when set, tells simplex-chat
// where to write the file; the path must not contain spaces (the command
// grammar is whitespace-delimited), which the caller guarantees by sanitising.
func buildReceiveCmd(fileID int64, destPath string) string {
	if destPath == "" {
		return fmt.Sprintf("/freceive %d", fileID)
	}
	return fmt.Sprintf("/freceive %d %s", fileID, destPath)
}

// deliverFile routes an rcvFileComplete/rcvFileError push to the ReceiveFile
// waiter registered for that fileId.
func (c *wsClient) deliverFile(raw json.RawMessage, recvErr error) {
	var resp struct {
		ChatItem struct {
			ChatItem struct {
				File *ciFileBlock `json:"file"`
			} `json:"chatItem"`
		} `json:"chatItem"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		c.log.Warn("simplex: decode file event", "err", err)
		return
	}
	if resp.ChatItem.ChatItem.File == nil {
		return
	}
	id := resp.ChatItem.ChatItem.File.FileID
	c.mu.Lock()
	ch, ok := c.fileWaiters[id]
	if ok {
		delete(c.fileWaiters, id)
	}
	abandonedPath, wasAbandoned := c.abandoned[id]
	if wasAbandoned {
		delete(c.abandoned, id)
	}
	c.mu.Unlock()
	if ok {
		ch <- recvErr
		close(ch)
		return
	}
	// No live waiter: ReceiveFile already returned. If it abandoned this fileId
	// on timeout/cancel, simplex-chat finished the write after we removed it (or
	// before we got a chance to); clean up the now-orphaned file. Both an error
	// and a "complete" land here as junk the caller was told it would not get.
	if wasAbandoned {
		c.removeReceivedFile(id, abandonedPath)
	}
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
