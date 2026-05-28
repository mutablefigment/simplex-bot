package bot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// sweepInterval is how often the inbox is scanned for expired files. The
// retention window (typically 30d) is the dominant clock; running hourly keeps
// the disk usage reasonably close to the cap without being chatty.
const sweepInterval = 1 * time.Hour

// runInboxSweeper periodically deletes files in cfg.Storage.InboxDir whose
// mtime is older than cfg.Storage.InboxRetention. It runs as a long-lived
// goroutine launched from Run and exits when ctx is cancelled.
//
// Disabled when retention <= 0 (e.g. tests that don't set it). The first
// sweep runs once at startup so a long-lived bot with stale files from a
// previous boot doesn't have to wait an hour to start cleaning up.
func (b *Bot) runInboxSweeper(ctx context.Context) {
	retention := time.Duration(b.cfg.Storage.InboxRetention)
	if retention <= 0 {
		b.log.Debug("inbox sweeper: disabled (retention <= 0)")
		return
	}
	dir := b.cfg.Storage.InboxDir
	if dir == "" {
		return
	}

	b.sweepInboxOnce(dir, retention)

	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.sweepInboxOnce(dir, retention)
		}
	}
}

func (b *Bot) sweepInboxOnce(dir string, retention time.Duration) {
	cutoff := time.Now().Add(-retention)
	deleted, scanned, err := sweepInbox(dir, cutoff)
	if err != nil {
		b.log.Warn("inbox sweep failed", "dir", dir, "err", err)
		return
	}
	if deleted > 0 {
		b.log.Info("inbox sweep", "scanned", scanned, "deleted", deleted, "retention", retention)
	} else {
		b.log.Debug("inbox sweep", "scanned", scanned, "deleted", 0)
	}
}

// sweepInbox is the pure-logic core: walk the top level of dir, delete regular
// files older than cutoff, and return counts. Symlinks are never followed and
// never deleted (defense in depth — the inbox is meant to hold user-uploaded
// regular files, and a symlink pointing out of the tree could be there only by
// misconfiguration or attack). Subdirectories are left alone (the spec only
// promises a flat layout under inbox/).
func sweepInbox(dir string, cutoff time.Time) (deleted, scanned int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read inbox: %w", err)
	}
	for _, e := range entries {
		scanned++
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil {
			// log-and-continue at the call site; here we just stop counting
			// the file as deleted.
			continue
		}
		deleted++
	}
	return deleted, scanned, nil
}
