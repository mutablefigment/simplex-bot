package claude

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"claude-bot/internal/config"
)

// TestRunner_Smoke spawns the real `claude` binary. Skipped unless
// CLAUDE_BOT_INTEGRATION=1 is set so `go test ./...` stays free.
func TestRunner_Smoke(t *testing.T) {
	if os.Getenv("CLAUDE_BOT_INTEGRATION") != "1" {
		t.Skip("set CLAUDE_BOT_INTEGRATION=1 to run (costs API tokens)")
	}
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "/home/sprite/.local/bin/claude"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("claude binary missing at %s", bin)
	}

	wd, _ := os.Getwd()
	cfg := config.Claude{
		Binary:    bin,
		Workspace: wd,
		Model:     "claude-haiku-4-5-20251001", // cheap path for smoke
	}
	r := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := r.Run(ctx, "reply with the single word 'pong'", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var (
		gotInit   bool
		text      strings.Builder
		gotResult bool
		resultErr error
	)
	for ev := range events {
		switch e := ev.(type) {
		case InitEvent:
			gotInit = true
			if e.SessionID == "" {
				t.Error("InitEvent.SessionID empty")
			}
		case AssistantTextEvent:
			text.WriteString(e.Text)
		case ResultEvent:
			gotResult = true
			resultErr = e.Error
		}
	}
	if !gotInit {
		t.Error("no InitEvent")
	}
	if !gotResult {
		t.Error("no ResultEvent")
	}
	if resultErr != nil {
		t.Errorf("result error: %v", resultErr)
	}
	if !strings.Contains(strings.ToLower(text.String()), "pong") {
		t.Errorf("expected 'pong' in response, got %q", text.String())
	}
}
