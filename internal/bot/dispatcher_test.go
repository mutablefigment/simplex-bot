package bot

import "testing"

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantName string
		wantArgs string
	}{
		{"/new", true, "new", ""},
		{"/help me please", true, "help", "me please"},
		{"   /new   ", true, "new", ""},
		{"hello", false, "", ""},
		{"", false, "", ""},
		{"/", false, "", ""},
		{"not /a command", false, "", ""},
	}
	for _, c := range cases {
		got, ok := parseCommand(c.in)
		if ok != c.wantOK || got.Name != c.wantName || got.Args != c.wantArgs {
			t.Errorf("parseCommand(%q) = (%+v, %v), want ({%s %s}, %v)",
				c.in, got, ok, c.wantName, c.wantArgs, c.wantOK)
		}
	}
}
