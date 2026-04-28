package config

import (
	"fmt"
	"os"

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
	return nil
}
