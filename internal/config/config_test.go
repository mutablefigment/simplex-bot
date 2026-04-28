package config

import (
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
