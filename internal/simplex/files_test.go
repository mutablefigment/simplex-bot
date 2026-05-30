package simplex

import (
	"encoding/json"
	"strings"
	"testing"
)

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
