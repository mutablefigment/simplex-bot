package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type streamFrame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID string `json:"session_id"`

	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`

	// Event is set on `stream_event` frames emitted with
	// --include-partial-messages: content_block_delta carries text_delta chunks
	// as the model produces them.
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`

	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
}

// parseStream reads stream-json frames from r and emits Events to out.
// It returns when r reaches EOF or ctx is cancelled. The caller is responsible
// for closing out and synthesising a terminal ResultEvent if none was emitted.
//
// Returns the *organic* ResultEvent if one was seen (Error=nil), otherwise nil.
// A non-nil io error is returned only on read failure; malformed lines are
// skipped with no error.
func parseStream(ctx context.Context, r io.Reader, out chan<- Event) (*ResultEvent, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<22)

	var (
		organic  *ResultEvent
		sawDelta bool // once a text_delta arrives, the cumulative `assistant` frame would double-count
	)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return organic, ctx.Err()
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var f streamFrame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}

		switch f.Type {
		case "system":
			if f.Subtype == "init" && f.SessionID != "" {
				select {
				case out <- InitEvent{SessionID: f.SessionID}:
				case <-ctx.Done():
					return organic, ctx.Err()
				}
			}
		case "stream_event":
			// content_block_delta with text_delta = incremental assistant text.
			// Other stream_event types (message_start, content_block_start,
			// thinking_delta, signature_delta, message_stop, ...) are ignored.
			if f.Event.Type == "content_block_delta" && f.Event.Delta.Type == "text_delta" && f.Event.Delta.Text != "" {
				sawDelta = true
				select {
				case out <- AssistantTextEvent{Text: f.Event.Delta.Text}:
				case <-ctx.Done():
					return organic, ctx.Err()
				}
			}
		case "assistant":
			for _, c := range f.Message.Content {
				switch c.Type {
				case "text":
					// Skip cumulative text frames once we've started delta mode.
					if sawDelta || c.Text == "" {
						continue
					}
					select {
					case out <- AssistantTextEvent{Text: c.Text}:
					case <-ctx.Done():
						return organic, ctx.Err()
					}
				case "tool_use":
					select {
					case out <- ToolUseEvent{Name: c.Name}:
					case <-ctx.Done():
						return organic, ctx.Err()
					}
				}
			}
		case "result":
			ev := ResultEvent{
				CostUSD:    f.TotalCostUSD,
				DurationMS: f.DurationMS,
			}
			if f.IsError {
				ev.Error = fmt.Errorf("%w: %s", ErrCrash, f.Result)
			}
			organic = &ev
			// Don't forward yet — runner emits the terminal ResultEvent after
			// the subprocess actually exits, so it can synthesise on crash.
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return organic, err
	}
	return organic, nil
}
