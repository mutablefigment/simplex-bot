package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"claude-bot/internal/config"
	"claude-bot/internal/simplex"
)

// dequeue pulls one job off the bot's queue or fails after a short wait.
func dequeue(t *testing.T, b *Bot) job {
	t.Helper()
	select {
	case j := <-b.queue:
		return j
	case <-time.After(time.Second):
		t.Fatal("no job was queued")
		return job{}
	}
}

func newAttachmentBot(t *testing.T, fake *fakeSimplex) *Bot {
	t.Helper()
	cfg := &config.Config{
		Simplex: config.Simplex{AllowedContactID: 42},
		Claude:  config.Claude{Workspace: t.TempDir()},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, log, fake, nil, nil)
}

func TestHandleChatItem_CaptionedAttachment(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    7,
		Text:      "look",
		Files:     []simplex.File{{ID: 9, Name: "pic.png", Size: 2048}},
	})

	j := dequeue(t, b)
	if j.slash != nil {
		t.Fatalf("captioned file was parsed as a slash command")
	}
	if !strings.HasPrefix(j.prompt, "look\n[attached: ./inbox/") || !strings.HasSuffix(j.prompt, "_pic.png]") {
		t.Errorf("prompt = %q, want caption + ./inbox/<ts>_pic.png attachment", j.prompt)
	}
	// The file was actually received into the inbox.
	if ops := fake.snapshotOps(); len(ops) == 0 || ops[0] != "receive_file" {
		t.Errorf("ReceiveFile not called; ops = %v", ops)
	}
}

func TestHandleChatItem_FileOnly(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    8,
		Text:      "", // no caption — must still be a valid prompt
		Files:     []simplex.File{{ID: 9, Name: "doc.pdf", Size: 10}},
	})

	j := dequeue(t, b)
	if !strings.HasPrefix(j.prompt, "[attached: ./inbox/") || !strings.HasSuffix(j.prompt, "_doc.pdf]") {
		t.Errorf("file-only prompt = %q, want a bare ./inbox attachment reference", j.prompt)
	}
}

func TestSafeFileName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{"/abs/path/x.png", "x.png"},
		{`..\..\windows\system32`, "system32"},
		{".hidden", "hidden"},
		{"...", "file"},
		{"", "file"},
		{"with space.txt", "with_space.txt"},
		{"tab\tand\nnewline", "tab_and_newline"},
		{"null\x00byte", "null_byte"},
	}
	for _, c := range cases {
		got := safeFileName(c.in)
		if got != c.want {
			t.Errorf("safeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "/\\") || strings.ContainsRune(got, 0) {
			t.Errorf("safeFileName(%q) = %q still contains a separator/null", c.in, got)
		}
		if strings.ContainsAny(got, " \t\n\r") {
			t.Errorf("safeFileName(%q) = %q still contains whitespace (breaks /freceive grammar)", c.in, got)
		}
	}
}

func TestSafeFileNameTruncates(t *testing.T) {
	long := strings.Repeat("a", 500) + ".txt"
	if got := safeFileName(long); len(got) > 128 {
		t.Errorf("safeFileName did not truncate: len=%d", len(got))
	}
}

func TestWithAttachments(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		refs    []string
		want    string
	}{
		{"caption only", "hello", nil, "hello"},
		{"file only", "", []string{"./inbox/1_a.png"}, "[attached: ./inbox/1_a.png]"},
		{"caption plus file", "look", []string{"./inbox/1_a.png"},
			"look\n[attached: ./inbox/1_a.png]"},
		{"multiple files", "", []string{"./inbox/1_a.png", "./inbox/1_b.pdf"},
			"[attached: ./inbox/1_a.png]\n[attached: ./inbox/1_b.pdf]"},
		{"nothing", "", nil, ""},
	}
	for _, c := range cases {
		if got := withAttachments(c.caption, c.refs); got != c.want {
			t.Errorf("%s: withAttachments(%q, %v) = %q, want %q", c.name, c.caption, c.refs, got, c.want)
		}
	}
}

func TestPromptPath(t *testing.T) {
	b := &Bot{cfg: &config.Config{Claude: config.Claude{Workspace: "/var/lib/claude-bot/workspace"}}}
	cases := []struct {
		in, want string
	}{
		{"/var/lib/claude-bot/workspace/inbox/1_a.png", "./inbox/1_a.png"},
		{"/somewhere/else/x.png", "/somewhere/else/x.png"},
	}
	for _, c := range cases {
		if got := b.promptPath(c.in); got != c.want {
			t.Errorf("promptPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
