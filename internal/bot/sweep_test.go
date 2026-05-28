package bot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepInbox_DeletesExpiredKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cutoff := now.Add(-7 * 24 * time.Hour)

	oldFile := filepath.Join(dir, "old.txt")
	freshFile := filepath.Join(dir, "fresh.txt")
	mustWrite(t, oldFile, "stale")
	mustWrite(t, freshFile, "current")
	mustChtimes(t, oldFile, cutoff.Add(-1*time.Hour))
	mustChtimes(t, freshFile, cutoff.Add(1*time.Hour))

	deleted, scanned, err := sweepInbox(dir, cutoff)
	if err != nil {
		t.Fatalf("sweepInbox: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("fresh file should remain, stat err = %v", err)
	}
}

func TestSweepInbox_IgnoresSymlinksAndSubdirs(t *testing.T) {
	dir := t.TempDir()
	cutoff := time.Now().Add(1 * time.Hour) // everything is "old"

	target := filepath.Join(t.TempDir(), "outside.txt")
	mustWrite(t, target, "do not touch")

	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("Mkdir subdir: %v", err)
	}
	inner := filepath.Join(subdir, "inner.txt")
	mustWrite(t, inner, "inside subdir")
	mustChtimes(t, inner, time.Unix(0, 0))

	if _, _, err := sweepInbox(dir, cutoff); err != nil {
		t.Fatalf("sweepInbox: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target was touched: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink itself was deleted: %v", err)
	}
	if _, err := os.Stat(inner); err != nil {
		t.Errorf("file in subdir was deleted: %v", err)
	}
}

func TestSweepInbox_MissingDir(t *testing.T) {
	deleted, scanned, err := sweepInbox(filepath.Join(t.TempDir(), "does-not-exist"), time.Now())
	if err != nil {
		t.Fatalf("expected nil err on missing dir, got %v", err)
	}
	if scanned != 0 || deleted != 0 {
		t.Errorf("counts = (%d,%d), want (0,0)", deleted, scanned)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func mustChtimes(t *testing.T, p string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
}
