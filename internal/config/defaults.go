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
	if c.Storage.MaxAttachmentSize == 0 {
		// 100 MiB: generous for documents/screenshots a single trusted contact
		// would send, while keeping a runaway/many-large transfer from filling
		// the inbox disk. Set to 0 in config to disable the cap.
		c.Storage.MaxAttachmentSize = ByteSize(100 << 20)
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
}
