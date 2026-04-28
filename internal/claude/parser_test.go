package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseStream_HappyPath(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc-123"}`,
		`{"type":"rate_limit_event","rate_limit_info":{}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" world"},{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"result":"hello world","session_id":"abc-123"}`,
	}, "\n")

	out := make(chan Event, 16)
	res, err := parseStream(context.Background(), strings.NewReader(in), out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	close(out)

	var got []Event
	for ev := range out {
		got = append(got, ev)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 events, got %d: %#v", len(got), got)
	}
	if init, ok := got[0].(InitEvent); !ok || init.SessionID != "abc-123" {
		t.Errorf("event[0] = %#v, want InitEvent{abc-123}", got[0])
	}
	if a, ok := got[1].(AssistantTextEvent); !ok || a.Text != "hello" {
		t.Errorf("event[1] = %#v, want AssistantTextEvent{hello}", got[1])
	}
	if a, ok := got[2].(AssistantTextEvent); !ok || a.Text != " world" {
		t.Errorf("event[2] = %#v, want AssistantTextEvent{ world}", got[2])
	}
	if tu, ok := got[3].(ToolUseEvent); !ok || tu.Name != "Read" {
		t.Errorf("event[3] = %#v, want ToolUseEvent{Read}", got[3])
	}

	if res == nil {
		t.Fatal("organic ResultEvent was nil")
	}
	if res.CostUSD != 0.05 || res.DurationMS != 1234 || res.Error != nil {
		t.Errorf("result = %#v", res)
	}
}

func TestParseStream_ResultIsError(t *testing.T) {
	in := `{"type":"result","subtype":"error","is_error":true,"result":"rate limit exceeded","duration_ms":5}`
	out := make(chan Event, 4)
	res, err := parseStream(context.Background(), strings.NewReader(in), out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	close(out)

	if res == nil || res.Error == nil {
		t.Fatalf("want result with error, got %#v", res)
	}
	if !errors.Is(res.Error, ErrCrash) {
		t.Errorf("want wrapped ErrCrash, got %v", res.Error)
	}
}

func TestParseStream_MalformedLineSkipped(t *testing.T) {
	in := strings.Join([]string{
		`not json`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
	}, "\n")
	out := make(chan Event, 4)
	if _, err := parseStream(context.Background(), strings.NewReader(in), out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	close(out)

	var n int
	for range out {
		n++
	}
	if n != 1 {
		t.Errorf("want 1 event, got %d", n)
	}
}

func TestParseStream_EmptyTextSkipped(t *testing.T) {
	in := `{"type":"assistant","message":{"content":[{"type":"text","text":""}]}}`
	out := make(chan Event, 4)
	if _, err := parseStream(context.Background(), strings.NewReader(in), out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	close(out)

	for ev := range out {
		t.Errorf("unexpected event: %#v", ev)
	}
}
