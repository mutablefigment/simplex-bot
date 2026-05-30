package simplex

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discardClient() *wsClient {
	return &wsClient{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		fileWaiters: make(map[int64]chan error),
		abandoned:   make(map[int64]string),
	}
}

// TestRemoveReceivedFile covers the best-effort cleanup helper used on the
// abandon/failure paths: it deletes an existing file, tolerates a missing one,
// and no-ops on an empty path.
func TestRemoveReceivedFile(t *testing.T) {
	c := discardClient()

	dir := t.TempDir()
	existing := filepath.Join(dir, "partial.bin")
	if err := os.WriteFile(existing, []byte("partial"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	c.removeReceivedFile(7, existing)
	if _, err := os.Stat(existing); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", existing, err)
	}

	// Missing file: must not panic or report (idempotent).
	c.removeReceivedFile(7, filepath.Join(dir, "never_existed.bin"))

	// Empty destPath (the /freceive-without-path case): no-op.
	c.removeReceivedFile(7, "")
}

// TestDeliverFile_LateCompletionCleansAbandoned exercises the post-timeout write
// race: ReceiveFile already gave up (no waiter) but registered destPath as
// abandoned, then simplex-chat finishes writing and emits a late, otherwise
// unrouted rcvFileComplete. deliverFile must remove the now-orphaned file and
// drop the bookkeeping.
func TestDeliverFile_LateCompletionCleansAbandoned(t *testing.T) {
	c := discardClient()

	dir := t.TempDir()
	orphan := filepath.Join(dir, "late.png")
	if err := os.WriteFile(orphan, []byte("bytes simplex wrote after timeout"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	c.markAbandoned(9, orphan)

	const ev = `{"type":"rcvFileComplete","chatItem":{"chatItem":{"file":{"fileId":9,"fileName":"late.png"}}}}`
	c.deliverFile(json.RawMessage(ev), nil)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("late completion did not remove orphan, stat err = %v", err)
	}
	c.mu.Lock()
	_, still := c.abandoned[9]
	c.mu.Unlock()
	if still {
		t.Fatal("abandoned bookkeeping not cleared after late completion")
	}
}

// TestDeliverFile_SuccessKeepsFile is the correctness guard: when a live waiter
// is present (the success path), deliverFile must route the completion to that
// waiter and must NOT remove destPath — the caller still has to read it.
func TestDeliverFile_SuccessKeepsFile(t *testing.T) {
	c := discardClient()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.png")
	if err := os.WriteFile(good, []byte("real payload"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	ch := make(chan error, 1)
	c.mu.Lock()
	c.fileWaiters[9] = ch
	c.mu.Unlock()

	const ev = `{"type":"rcvFileComplete","chatItem":{"chatItem":{"file":{"fileId":9,"fileName":"good.png"}}}}`
	c.deliverFile(json.RawMessage(ev), nil)

	select {
	case e := <-ch:
		if e != nil {
			t.Fatalf("success completion delivered error: %v", e)
		}
	default:
		t.Fatal("completion was not routed to the live waiter")
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("success path must keep destPath, stat err = %v", err)
	}
}

func TestBuildReceiveCmd(t *testing.T) {
	if got, want := buildReceiveCmd(7, "/inbox/1700000000_a.png"), "/freceive 7 /inbox/1700000000_a.png"; got != want {
		t.Errorf("buildReceiveCmd = %q, want %q", got, want)
	}
	if got, want := buildReceiveCmd(7, ""), "/freceive 7"; got != want {
		t.Errorf("buildReceiveCmd (no path) = %q, want %q", got, want)
	}
	// The command grammar is whitespace-delimited; a path with control chars
	// would corrupt it. The bot sanitises filenames, but assert the contract.
	cmd := buildReceiveCmd(7, "/inbox/clean.png")
	if strings.ContainsAny(cmd[len("/freceive 7 "):], "\n\r\x00") {
		t.Errorf("buildReceiveCmd leaked a control char: %q", cmd)
	}
}

// TestDecodeInboundFile checks the CIFile sibling block is decoded and that the
// event-population gate only fires for files that still need receiving.
func TestDecodeInboundFile(t *testing.T) {
	const frame = `{
	  "chatItems": [{
	    "chatInfo": {"contact": {"contactId": 4}},
	    "chatItem": {
	      "chatDir": {"type": "directRcv"},
	      "content": {"msgContent": {"type": "image", "text": "see this"}},
	      "meta": {"itemId": 14},
	      "file": {"fileId": 9, "fileName": "pic.png", "fileSize": 2048,
	               "fileStatus": {"type": "rcvInvitation"}}
	    }
	  }]
	}`
	var resp struct {
		ChatItems []chatItemBlock `json:"chatItems"`
	}
	if err := json.Unmarshal([]byte(frame), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	it := resp.ChatItems[0].ChatItem
	if it.File == nil {
		t.Fatal("file block not decoded")
	}
	if it.File.FileID != 9 || it.File.FileName != "pic.png" || it.File.FileSize != 2048 {
		t.Errorf("file fields wrong: %+v", it.File)
	}
	if it.File.FileStatus.Type != "rcvInvitation" {
		t.Errorf("status = %q, want rcvInvitation", it.File.FileStatus.Type)
	}
	// Caption survives alongside the file.
	if it.Content.MsgContent.Text != "see this" {
		t.Errorf("caption = %q, want %q", it.Content.MsgContent.Text, "see this")
	}
}

// TestDecodeFileCompleteEvent mirrors what deliverFile parses out of an
// rcvFileComplete push to find the fileId.
func TestDecodeFileCompleteEvent(t *testing.T) {
	const ev = `{
	  "type": "rcvFileComplete",
	  "chatItem": {
	    "chatInfo": {"contact": {"contactId": 4}},
	    "chatItem": {"file": {"fileId": 9, "fileName": "pic.png"}}
	  }
	}`
	var resp struct {
		ChatItem struct {
			ChatItem struct {
				File *ciFileBlock `json:"file"`
			} `json:"chatItem"`
		} `json:"chatItem"`
	}
	if err := json.Unmarshal([]byte(ev), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ChatItem.ChatItem.File == nil || resp.ChatItem.ChatItem.File.FileID != 9 {
		t.Fatalf("fileId not found in rcvFileComplete: %+v", resp.ChatItem.ChatItem.File)
	}
}
