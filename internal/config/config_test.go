package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	cfg, err := Load("testdata/example.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Simplex.WSURL != "ws://127.0.0.1:5225" {
		t.Errorf("ws_url = %q", cfg.Simplex.WSURL)
	}
	if cfg.Simplex.AllowedContactID != 1 {
		t.Errorf("allowed_contact_id = %d", cfg.Simplex.AllowedContactID)
	}
	if got := time.Duration(cfg.Claude.TurnTimeout); got != 30*time.Minute {
		t.Errorf("turn_timeout = %v, want 30m", got)
	}
	if got := time.Duration(cfg.LiveMessage.UpdateInterval); got != 3*time.Second {
		t.Errorf("update_interval = %v, want 3s", got)
	}
	if got := time.Duration(cfg.Storage.InboxRetention); got != 720*time.Hour {
		t.Errorf("inbox_retention = %v, want 720h", got)
	}
	if got := int64(cfg.Storage.MaxAttachmentSize); got != 100<<20 {
		t.Errorf("max_attachment_size = %d, want %d (100MiB)", got, 100<<20)
	}
	if cfg.LiveMessage.ChunkThreshold != 4096 {
		t.Errorf("chunk_threshold = %d", cfg.LiveMessage.ChunkThreshold)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	// Write a minimal TOML and confirm defaults fill in.
	path := t.TempDir() + "/min.toml"
	min := `
[simplex]
ws_url = "ws://x"
allowed_contact_id = 42

[claude]
binary = "/x"
workspace = "/y"

[storage]
db_path = "/z"
`
	if err := writeFile(path, min); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := time.Duration(cfg.Claude.TurnTimeout); got != 30*time.Minute {
		t.Errorf("default turn_timeout = %v", got)
	}
	if got := time.Duration(cfg.LiveMessage.UpdateInterval); got != 3*time.Second {
		t.Errorf("default update_interval = %v", got)
	}
	if got := int64(cfg.Storage.MaxAttachmentSize); got != 100<<20 {
		t.Errorf("default max_attachment_size = %d, want %d (100MiB)", got, 100<<20)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log.level = %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("default log.format = %q", cfg.Log.Format)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	path := t.TempDir() + "/bad.toml"
	if err := writeFile(path, `[simplex]`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

// loadWith writes a minimal valid config with the given claude.workspace and
// storage.inbox_dir (inbox_dir is omitted when empty) and returns the Load
// error, exercising Validate's effective-inbox whitespace check (issue #29).
func loadWith(t *testing.T, workspace, inboxDir string) error {
	t.Helper()
	cfg := "[simplex]\n" +
		"ws_url = \"ws://x\"\n" +
		"allowed_contact_id = 42\n\n" +
		"[claude]\n" +
		"binary = \"/x\"\n" +
		"workspace = \"" + workspace + "\"\n\n" +
		"[storage]\n" +
		"db_path = \"/z\"\n"
	if inboxDir != "" {
		cfg += "inbox_dir = \"" + inboxDir + "\"\n"
	}
	path := t.TempDir() + "/c.toml"
	if err := writeFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

// TestValidateInboxWhitespace asserts the effective inbox path is rejected when
// it contains whitespace (the /freceive grammar is whitespace-delimited, #29):
// storage.inbox_dir when set, otherwise claude.workspace. Clean paths pass, and
// the error names the offending field.
func TestValidateInboxWhitespace(t *testing.T) {
	cases := []struct {
		name      string
		workspace string
		inboxDir  string
		wantField string // "" means expect no error
	}{
		{"clean workspace, no inbox_dir", "/srv/claude-bot", "", ""},
		{"clean inbox_dir", "/srv/claude-bot", "/srv/inbox", ""},
		{"space in workspace feeds inbox", "/srv/claude bot", "", "claude.workspace"},
		{"space in inbox_dir", "/srv/claude-bot", "/srv/claude bot/inbox", "storage.inbox_dir"},
		{"tab in inbox_dir", "/srv/claude-bot", "/srv/in\\tbox", "storage.inbox_dir"},
		{"newline in inbox_dir", "/srv/claude-bot", "/srv/in\\nbox", "storage.inbox_dir"},
		{"tab in workspace feeds inbox", "/srv/cl\\taud", "", "claude.workspace"},
		// inbox_dir overrides workspace: a space in workspace is irrelevant to
		// /freceive once inbox_dir (clean) is set, so it must NOT be rejected.
		{"dirty workspace ignored when inbox_dir set", "/srv/claude bot", "/srv/inbox", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := loadWith(t, tc.workspace, tc.inboxDir)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("expected clean config to load, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name offending field %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestContainsWhitespace pins the helper to the full set of whitespace runes the
// issue calls out (space, tab, CR, LF, vertical tab, form feed) plus a clean path.
func TestContainsWhitespace(t *testing.T) {
	dirty := []string{"a b", "a\tb", "a\rb", "a\nb", "a\vb", "a\fb", "a b"}
	for _, s := range dirty {
		if !containsWhitespace(s) {
			t.Errorf("containsWhitespace(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "/srv/claude-bot/inbox", "no_whitespace_here"} {
		if containsWhitespace(s) {
			t.Errorf("containsWhitespace(%q) = true, want false", s)
		}
	}
}
