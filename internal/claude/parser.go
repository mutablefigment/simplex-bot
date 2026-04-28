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

	var organic *ResultEvent
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
		case "assistant":
			for _, c := range f.Message.Content {
				switch c.Type {
				case "text":
					if c.Text == "" {
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
