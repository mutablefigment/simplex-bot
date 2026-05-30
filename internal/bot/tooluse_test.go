package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-bot/internal/claude"
	"claude-bot/internal/config"
	"claude-bot/internal/simplex"
	"claude-bot/internal/store"
)

// fakeRunner is an in-memory claude.Runner that replays a fixed script of
// events on every Run call. It honours the Runner contract: the script must end
// in a ResultEvent and the returned channel is closed afterwards.
type fakeRunner struct {
	script []claude.Event
}

func (f *fakeRunner) Run(ctx context.Context, prompt, sessionID string) (<-chan claude.Event, error) {
	out := make(chan claude.Event, len(f.script))
	for _, ev := range f.script {
		out <- ev
	}
	close(out)
	return out, nil
}

// runToolUseTurn drives the real bot.runTurn through a scripted claude run with
// ShowToolUse set to showToolUse, and returns the text of the finalise op
// (which carries the full composed reply). The live-message update interval is
// set very short so a flush fires before the turn ends, exercising the same
// streaming path production uses.
func runToolUseTurn(t *testing.T, showToolUse bool, script []claude.Event) string {
	t.Helper()
	const allowedCID = int64(42)
	tmp := t.TempDir()

	cfg := &config.Config{
		Simplex: config.Simplex{AllowedContactID: allowedCID},
		Claude: config.Claude{
			Workspace:   tmp,
			ShowToolUse: showToolUse,
			TurnTimeout: config.Duration(30 * time.Second),
		},
		LiveMessage: config.LiveMessage{
			UpdateInterval: config.Duration(5 * time.Millisecond),
			ChunkThreshold: 4096,
		},
	}

	st, err := store.Open(context.Background(), filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := newFakeSimplex()
	b := New(cfg, log, "test", fake, &fakeRunner{script: script}, st)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runTurn(ctx, job{
			prompt:    "do the thing",
			itemID:    100,
			contactID: allowedCID,
		})
	}()

	final := fake.waitOp(t, "finalise", 5*time.Second)
	<-done
	return final.text
}

func TestRunTurn_ShowToolUse_On_IndicatorPresent(t *testing.T) {
	script := []claude.Event{
		claude.InitEvent{SessionID: "s1"},
		claude.AssistantTextEvent{Text: "Let me look."},
		claude.ToolUseEvent{Name: "Read"},
		claude.AssistantTextEvent{Text: " All set."},
		claude.ResultEvent{CostUSD: 0, DurationMS: 0},
	}
	got := runToolUseTurn(t, true, script)
	want := "Let me look.\n🔧 Read\n All set."
	if got != want {
		t.Fatalf("flag on: finalise text = %q, want %q", got, want)
	}
}

func TestRunTurn_ShowToolUse_On_ToolBeforeText(t *testing.T) {
	// Ordering case: tool use arrives before ANY assistant text. The live
	// message must still open (lazy-open) and carry the indicator first.
	script := []claude.Event{
		claude.InitEvent{SessionID: "s1"},
		claude.ToolUseEvent{Name: "Bash"},
		claude.AssistantTextEvent{Text: "Build is green."},
		claude.ResultEvent{},
	}
	got := runToolUseTurn(t, true, script)
	want := "🔧 Bash\nBuild is green."
	if got != want {
		t.Fatalf("tool-before-text: finalise text = %q, want %q", got, want)
	}
}

func TestRunTurn_ShowToolUse_On_DedupesConsecutive(t *testing.T) {
	// claude fires Read three times in a row, then Bash. The consecutive Reads
	// collapse to one indicator; Bash gets its own line.
	script := []claude.Event{
		claude.InitEvent{SessionID: "s1"},
		claude.ToolUseEvent{Name: "Read"},
		claude.ToolUseEvent{Name: "Read"},
		claude.ToolUseEvent{Name: "Read"},
		claude.ToolUseEvent{Name: "Bash"},
		claude.AssistantTextEvent{Text: "ok"},
		claude.ResultEvent{},
	}
	got := runToolUseTurn(t, true, script)
	want := "🔧 Read\n🔧 Bash\nok"
	if got != want {
		t.Fatalf("dedup: finalise text = %q, want %q", got, want)
	}
}

func TestRunTurn_ShowToolUse_Off_IndicatorAbsent(t *testing.T) {
	// Same script as the On case, but the default (false) flag must leave the
	// reply identical to text-only — no indicator, no extra lines.
	script := []claude.Event{
		claude.InitEvent{SessionID: "s1"},
		claude.AssistantTextEvent{Text: "Let me look."},
		claude.ToolUseEvent{Name: "Read"},
		claude.AssistantTextEvent{Text: " All set."},
		claude.ResultEvent{},
	}
	got := runToolUseTurn(t, false, script)
	want := "Let me look. All set."
	if got != want {
		t.Fatalf("flag off: finalise text = %q, want %q", got, want)
	}
	if strings.Contains(got, "🔧") {
		t.Errorf("flag off but indicator present: %q", got)
	}
}

// Guard: the simplex.Client and claude.Runner fakes satisfy the real
// interfaces the bot depends on, so these tests exercise the production wiring.
var (
	_ simplex.Client = (*fakeSimplex)(nil)
	_ claude.Runner  = (*fakeRunner)(nil)
)
