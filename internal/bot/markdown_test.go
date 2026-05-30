package bot

import "testing"

func TestTranslateMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// trivial
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"newlines preserved", "a\nb\nc", "a\nb\nc"},
		{"blank line", "a\n\nb", "a\n\nb"},

		// bold
		{"bold simple", "**bold**", "*bold*"},
		{"bold in sentence", "say **hello** world", "say *hello* world"},
		{"two bolds", "**a** and **b**", "*a* and *b*"},
		{"unclosed bold", "**oops", "**oops"},

		// italic
		{"italic simple", "*italic*", "_italic_"},
		{"italic in sentence", "say *hi* there", "say _hi_ there"},
		{"two italics", "*x* *y*", "_x_ _y_"},
		{"unclosed italic", "*nope", "*nope"},

		// literal asterisks in prose must NOT become italics (issue #35)
		{"literal asterisk multiply spaced", "use a * b and c * d", "use a * b and c * d"},
		{"literal asterisk globs", "files: *.go and *.md", "files: *.go and *.md"},
		{"literal asterisk product chain", "the product a * b * c", "the product a * b * c"},
		{"literal asterisk single spaced pair", "a * b", "a * b"},
		{"literal asterisk leading glob only", "*.go files", "*.go files"},
		{"italic still works among literals", "use a * b then *really* fast", "use a * b then _really_ fast"},
		{"bold survives among literal asterisks", "a * b and **bold** c", "a * b and *bold* c"},

		// bold + italic interactions (the load-bearing case)
		{"bold then italic", "**a** *b*", "*a* _b_"},
		{"italic then bold", "*a* **b**", "_a_ *b*"},
		{"italic inside bold", "**a *b* c**", "*a _b_ c*"},
		{"nested italic in bold regression (#35)", "**a *b* c**", "*a _b_ c*"},
		{"bold simple regression (#35)", "**bold**", "*bold*"},
		{"italic simple regression (#35)", "*italic*", "_italic_"},
		{"bold not clobbered by italic pass", "**bold**", "*bold*"},
		{"adjacent bold and italic", "**bold***italic*", "*bold*_italic_"},

		// strike
		{"strike simple", "~~gone~~", "~gone~"},
		{"strike in sentence", "and ~~done~~ now", "and ~done~ now"},

		// heading
		{"h1", "# Title", "*Title*"},
		{"h2", "## Subtitle", "*Subtitle*"},
		{"h3", "### Deep", "*Deep*"},
		{"hash without space stays text", "#hashtag", "#hashtag"},
		{"heading with italic body", "# Hello *world*", "*Hello _world_*"},

		// link / image
		{"link basic", "[GitHub](https://github.com)", "GitHub (https://github.com)"},
		{"link in sentence", "see [docs](https://x.io) please", "see docs (https://x.io) please"},
		{"image untouched", "![alt](https://example.com/x.png)", "![alt](https://example.com/x.png)"},
		{"link not preceded by bang", "wait! [docs](url)", "wait! docs (url)"},

		// code span preservation
		{"code span untouched", "use `*ptr` carefully", "use `*ptr` carefully"},
		{"code span with bold marker", "`**not bold**` here", "`**not bold**` here"},
		{"transform around code span", "*hi* `*kept*` *bye*", "_hi_ `*kept*` _bye_"},

		// fenced blocks
		{
			"fenced block: each line wrapped in single backticks",
			"before\n```\ninside\n```\nafter",
			"before\n`inside`\nafter",
		},
		{
			"fenced lang tag dropped, lines wrapped",
			"```go\nfn()\n```",
			"`fn()`",
		},
		{
			"multiline fenced block",
			"```\nline a\nline b\n```",
			"`line a`\n`line b`",
		},
		{
			"transforms suppressed inside fence (bold marker passes through wrapped)",
			"```\n**not bold**\n```",
			"`**not bold**`",
		},
		{
			"empty fenced line preserved",
			"```\n\n```",
			"",
		},
		{
			"backtick-in-code passes through plain (no nested span)",
			"```\nuse `foo` here\n```",
			"use `foo` here",
		},
		{
			"unclosed fence: lines wrapped as code",
			"```\n**raw**\nstill raw",
			"`**raw**`\n`still raw`",
		},

		// streaming partial markup — must be left alone
		{"partial bold opener", "Hello **wor", "Hello **wor"},
		{"partial link", "see [docs", "see [docs"},
		{"backticks mid-line not fence", "use ``` to fence", "use ``` to fence"},

		// suffixes that the FSM appends — must round-trip cleanly (markdown-free)
		{"cost footer untouched", "answer\n\n— $0.0042 · 3.1s", "answer\n\n— $0.0042 · 3.1s"},
		{"interrupted tag untouched", "partial\n\n⚠️ interrupted", "partial\n\n⚠️ interrupted"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateMarkdown(tc.in)
			if got != tc.want {
				t.Errorf("translateMarkdown(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
