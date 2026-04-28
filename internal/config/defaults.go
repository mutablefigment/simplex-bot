package config

import "time"

func applyDefaults(c *Config) {
	if c.LiveMessage.UpdateInterval == 0 {
		c.LiveMessage.UpdateInterval = Duration(3 * time.Second)
	}
	if c.LiveMessage.ChunkThreshold == 0 {
		c.LiveMessage.ChunkThreshold = 4096
	}
	if c.Claude.TurnTimeout == 0 {
		c.Claude.TurnTimeout = Duration(30 * time.Minute)
	}
	if c.Storage.InboxRetention == 0 {
		c.Storage.InboxRetention = Duration(30 * 24 * time.Hour)
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
}
