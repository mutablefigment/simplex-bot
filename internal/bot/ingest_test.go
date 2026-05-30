package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
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
	return New(cfg, log, "test", fake, nil, nil)
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

// TestIngestFiles_SameNameDistinctPaths covers issue #30: two attachments in
// one message whose names are identical must land at distinct dest paths (no
// overwrite) and both must be referenced in the resulting prompt.
func TestIngestFiles_SameNameDistinctPaths(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    11,
		Text:      "two of the same",
		Files: []simplex.File{
			{ID: 100, Name: "report.pdf", Size: 1},
			{ID: 101, Name: "report.pdf", Size: 2},
		},
	})

	j := dequeue(t, b)

	// Both attachments referenced in the prompt.
	if n := strings.Count(j.prompt, "[attached: ./inbox/"); n != 2 {
		t.Fatalf("prompt = %q, want 2 attachment references, got %d", j.prompt, n)
	}

	// The two dest paths handed to ReceiveFile must differ.
	dests := receiveDests(fake)
	if len(dests) != 2 {
		t.Fatalf("ReceiveFile called %d times, want 2; dests = %v", len(dests), dests)
	}
	if dests[0] == dests[1] {
		t.Errorf("same-name attachments collided on dest %q", dests[0])
	}
	for _, d := range dests {
		assertNoWhitespace(t, d)
		// fileId is embedded so each path is unambiguous.
		if !strings.HasSuffix(d, "_report.pdf") {
			t.Errorf("dest %q lost its sanitised name suffix", d)
		}
	}
	if !strings.Contains(dests[0], "_100_") || !strings.Contains(dests[1], "_101_") {
		t.Errorf("dests %v do not each embed their fileId", dests)
	}
}

// TestIngestFiles_BothSanitiseToFile covers the sneakier collision: two names
// that are different on the wire but both reduce to "file" via safeFileName
// (e.g. all-dots / all-control-chars). They must still get distinct paths.
func TestIngestFiles_BothSanitiseToFile(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    12,
		Text:      "",
		Files: []simplex.File{
			{ID: 7, Name: "...", Size: 1}, // all dots -> "" after trim -> "file"
			{ID: 8, Name: "", Size: 2},    // empty name -> "file"
		},
	})

	j := dequeue(t, b)
	if n := strings.Count(j.prompt, "[attached: ./inbox/"); n != 2 {
		t.Fatalf("prompt = %q, want 2 attachment references, got %d", j.prompt, n)
	}

	dests := receiveDests(fake)
	if len(dests) != 2 {
		t.Fatalf("ReceiveFile called %d times, want 2; dests = %v", len(dests), dests)
	}
	if dests[0] == dests[1] {
		t.Errorf("both-sanitise-to-file attachments collided on dest %q", dests[0])
	}
	for _, d := range dests {
		assertNoWhitespace(t, d)
		if !strings.HasSuffix(d, "_file") {
			t.Errorf("dest %q did not fall back to the _file suffix", d)
		}
	}
}

// TestIngestFiles_OversizedSkipped covers issue #33: an attachment whose wire
// size exceeds storage.max_attachment_size is skipped (and the user notified)
// before ReceiveFile is called, while an under-cap sibling in the same message
// is still received and referenced in the prompt.
func TestIngestFiles_OversizedSkipped(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)
	b.cfg.Storage.MaxAttachmentSize = config.ByteSize(1000) // 1000-byte cap

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    13,
		Text:      "here",
		Files: []simplex.File{
			{ID: 200, Name: "huge.bin", Size: 5000}, // over the cap -> skipped
			{ID: 201, Name: "small.txt", Size: 10},  // under the cap -> received
		},
	})

	j := dequeue(t, b)

	// Only the under-cap file is referenced in the prompt.
	if n := strings.Count(j.prompt, "[attached: ./inbox/"); n != 1 {
		t.Fatalf("prompt = %q, want exactly 1 attachment reference", j.prompt)
	}
	if !strings.HasSuffix(j.prompt, "_small.txt]") {
		t.Errorf("prompt = %q, want the under-cap small.txt referenced", j.prompt)
	}
	if strings.Contains(j.prompt, "huge.bin") {
		t.Errorf("prompt = %q, oversized huge.bin must not be referenced", j.prompt)
	}

	// ReceiveFile was called exactly once, for the under-cap file only.
	dests := receiveDests(fake)
	if len(dests) != 1 {
		t.Fatalf("ReceiveFile called %d times, want 1; dests = %v", len(dests), dests)
	}
	if !strings.HasSuffix(dests[0], "_small.txt") {
		t.Errorf("received dest %q, want the under-cap small.txt", dests[0])
	}
	if !strings.Contains(dests[0], "_201_") {
		t.Errorf("received dest %q did not embed the under-cap fileId 201", dests[0])
	}

	// The user was notified about the skipped oversized attachment.
	var warned bool
	fake.mu.Lock()
	for _, s := range fake.sends {
		if s.op == "send" && strings.Contains(s.text, "huge.bin") && strings.Contains(s.text, "too large") {
			warned = true
		}
	}
	fake.mu.Unlock()
	if !warned {
		t.Errorf("no 'too large' notice sent for the skipped attachment")
	}
}

// TestIngestFiles_UnlimitedWhenZero confirms a 0 cap means unlimited: even a
// very large attachment is received.
func TestIngestFiles_UnlimitedWhenZero(t *testing.T) {
	fake := newFakeSimplex()
	b := newAttachmentBot(t, fake)
	b.cfg.Storage.MaxAttachmentSize = 0 // unlimited

	b.handleChatItem(context.Background(), simplex.ChatItemsEvent{
		ContactID: 42,
		ItemID:    14,
		Text:      "",
		Files:     []simplex.File{{ID: 300, Name: "big.bin", Size: 1 << 40}},
	})

	j := dequeue(t, b)
	if n := strings.Count(j.prompt, "[attached: ./inbox/"); n != 1 {
		t.Fatalf("prompt = %q, want the large attachment received under a 0 (unlimited) cap", j.prompt)
	}
	if dests := receiveDests(fake); len(dests) != 1 {
		t.Fatalf("ReceiveFile called %d times, want 1; dests = %v", len(dests), dests)
	}
}

// receiveDests returns, in order, the destPath argument of every ReceiveFile
// call recorded by the fake. The fake stores destPath in sentMsg.text.
func receiveDests(f *fakeSimplex) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var dests []string
	for _, s := range f.sends {
		if s.op == "receive_file" {
			dests = append(dests, s.text)
		}
	}
	return dests
}

func assertNoWhitespace(t *testing.T, path string) {
	t.Helper()
	// The base name (the component we generate) must carry no whitespace, so
	// the whole reference survives the space-delimited /freceive grammar.
	base := filepath.Base(path)
	if strings.ContainsAny(base, " \t\n\r\v\f") {
		t.Errorf("generated name %q contains whitespace (breaks /freceive grammar)", base)
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
