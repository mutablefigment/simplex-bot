package config

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Simplex     Simplex     `toml:"simplex"`
	Claude      Claude      `toml:"claude"`
	Storage     Storage     `toml:"storage"`
	LiveMessage LiveMessage `toml:"live_message"`
	Log         Log         `toml:"log"`
}

type Simplex struct {
	WSURL            string `toml:"ws_url"`
	AllowedContactID int64  `toml:"allowed_contact_id"`
}

type Claude struct {
	Binary          string   `toml:"binary"`
	Workspace       string   `toml:"workspace"`
	Model           string   `toml:"model"`
	AllowedTools    []string `toml:"allowed_tools"`
	DisallowedTools []string `toml:"disallowed_tools"`
	ShowToolUse     bool     `toml:"show_tool_use"`
	ShowCostFooter  bool     `toml:"show_cost_footer"`
	TurnTimeout     Duration `toml:"turn_timeout"`
}

type Storage struct {
	DBPath         string   `toml:"db_path"`
	InboxDir       string   `toml:"inbox_dir"`
	InboxRetention Duration `toml:"inbox_retention"`
	// MaxAttachmentSize caps the on-the-wire size of an inbound attachment the
	// bot will download into the inbox. 0 means unlimited; a positive value
	// causes oversized attachments to be skipped (and the user notified) before
	// the transfer starts. Defaults to 100MiB (see applyDefaults).
	MaxAttachmentSize ByteSize `toml:"max_attachment_size"`
}

type LiveMessage struct {
	UpdateInterval Duration `toml:"update_interval"`
	ChunkThreshold int      `toml:"chunk_threshold"`
}

type Log struct {
	Level           string `toml:"level"`
	Format          string `toml:"format"`
	LogFullMessages bool   `toml:"log_full_messages"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Simplex.WSURL == "" {
		return fmt.Errorf("simplex.ws_url is required")
	}
	if c.Simplex.AllowedContactID == 0 {
		return fmt.Errorf("simplex.allowed_contact_id is required")
	}
	if c.Claude.Binary == "" {
		return fmt.Errorf("claude.binary is required")
	}
	if c.Claude.Workspace == "" {
		return fmt.Errorf("claude.workspace is required")
	}
	if c.Storage.DBPath == "" {
		return fmt.Errorf("storage.db_path is required")
	}
	// The inbox directory is interpolated into the `/freceive <fileId> <path>`
	// command (internal/simplex/ws.go buildReceiveCmd), whose grammar is
	// whitespace-delimited. Any whitespace in the inbox path makes simplex-chat
	// truncate the destination and write attachments to the wrong place, so the
	// bot can never download files (issue #29). Reject it at load time with an
	// actionable message naming the exact offending field, rather than failing
	// opaquely per-attachment at runtime.
	//
	// Validate the ACTUAL effective inbox path: storage.inbox_dir when set,
	// otherwise claude.workspace (which becomes workspace/inbox — see
	// internal/bot/bot.go ingestFiles). We do not reject whitespace in
	// claude.workspace when storage.inbox_dir is set and overrides it: only the
	// path that actually feeds /freceive is load-bearing for this bug, and a
	// space elsewhere in the workspace tree is the operator's prerogative.
	if c.Storage.InboxDir != "" {
		if containsWhitespace(c.Storage.InboxDir) {
			return fmt.Errorf("storage.inbox_dir must not contain whitespace: %q (the inbox path is passed to the whitespace-delimited /freceive command, which would truncate it and misplace attachments)", c.Storage.InboxDir)
		}
	} else if containsWhitespace(c.Claude.Workspace) {
		return fmt.Errorf("claude.workspace must not contain whitespace: %q (with storage.inbox_dir unset the inbox becomes workspace/inbox, which is passed to the whitespace-delimited /freceive command and would be truncated, misplacing attachments)", c.Claude.Workspace)
	}
	return nil
}

// containsWhitespace reports whether s contains any whitespace rune (space, tab,
// CR, LF, vertical tab, form feed, and other Unicode spaces). Used to reject
// inbox paths the whitespace-delimited /freceive command cannot carry intact.
func containsWhitespace(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
}
