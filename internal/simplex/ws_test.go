package simplex

import (
	"strings"
	"testing"
)

// Verifies that user-controllable text never produces a raw newline byte in the
// command string, which would otherwise let simplex-chat reparse the remainder
// as a fresh CLI command. See issue #3.
func TestBuildSendCmd_NoControlByteEscape(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"newline", "hello\nworld"},
		{"crlf", "hello\r\nworld"},
		{"injected_command", "ok\n/_send @1 text rm -rf /"},
		{"null_byte", "tricky\x00stuff"},
		{"quote", `"quoted"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, quoted := range []int64{0, 42} {
				cmd := buildSendCmd(1, tt.text, quoted, false)
				if strings.ContainsAny(cmd, "\n\r\x00") {
					t.Fatalf("cmd contained a control byte: %q", cmd)
				}
				if !strings.HasPrefix(cmd, "/_send @1 json ") {
					t.Fatalf("expected json form, got: %q", cmd)
				}
			}
		})
	}
}

func TestBuildUpdateCmd_NoControlByteEscape(t *testing.T) {
	for _, text := range []string{"a\nb", "x\r\ny", "ok\n/_send @1 text pwned"} {
		for _, live := range []bool{true, false} {
			cmd := buildUpdateCmd(1, 2, text, live)
			if strings.ContainsAny(cmd, "\n\r\x00") {
				t.Fatalf("cmd contained a control byte (live=%v): %q", live, cmd)
			}
			if !strings.Contains(cmd, " json ") {
				t.Fatalf("expected json form, got: %q", cmd)
			}
			if live && !strings.Contains(cmd, "live=on") {
				t.Fatalf("missing live=on flag: %q", cmd)
			}
			if !live && strings.Contains(cmd, "live=on") {
				t.Fatalf("unexpected live=on flag: %q", cmd)
			}
		}
	}
}

func TestBuildSendCmd_QuotedShape(t *testing.T) {
	cmd := buildSendCmd(7, "hi", 99, true)
	want := `/_send @7 live=on json [{"msgContent":{"text":"hi","type":"text"},"quotedItemId":99}]`
	if cmd != want {
		t.Fatalf("got %q\nwant %q", cmd, want)
	}
}

func TestBuildSendCmd_UnquotedShape(t *testing.T) {
	cmd := buildSendCmd(7, "hi", 0, false)
	want := `/_send @7 json [{"msgContent":{"text":"hi","type":"text"}}]`
	if cmd != want {
		t.Fatalf("got %q\nwant %q", cmd, want)
	}
}
