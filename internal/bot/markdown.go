package bot

import (
	"regexp"
	"strings"
)

// translateMarkdown maps CommonMark-ish markup (what claude emits) to the
// SimpleX dialect:
//
//	**bold**         → *bold*
//	*italic*         → _italic_
//	~~strike~~       → ~strike~
//	# heading        → *heading*
//	```lang …```     → each fenced line wrapped in single backticks
//	[text](url)      → text (url)
//
// SimpleX has no native code-block rendering but DOES render single-backtick
// inline code as monospace — so each line of a fenced block is wrapped in
// single backticks. Lines that already contain a backtick pass through plain
// to avoid producing confusing nested-backtick spans. The opening/closing
// fence lines themselves are dropped.
//
// Code spans (`...`) and fenced-block contents are otherwise preserved —
// inline transforms (bold/italic/etc.) are suppressed inside both. Image
// syntax `![alt](url)` is left alone via a leading-`!` guard.
//
// The translator is intentionally conservative — partial markup (e.g. `**fo`
// while streaming) is left untouched and will translate on a later pass once
// the closing marker arrives.
func translateMarkdown(s string) string {
	if s == "" {
		return s
	}
	var (
		out     []string
		inFence bool
	)
	for _, line := range strings.Split(s, "\n") {
		if inFence {
			if isFenceLine(line) {
				inFence = false
				continue
			}
			out = append(out, wrapCodeLine(line))
			continue
		}
		if isFenceLine(line) {
			inFence = true
			continue
		}
		out = append(out, translateLine(line))
	}
	return strings.Join(out, "\n")
}

func isFenceLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// wrapCodeLine wraps a fenced-block line in single backticks so SimpleX
// renders it as inline monospace. Empty lines and lines that already contain
// a backtick pass through plain to avoid producing confusing nested spans.
func wrapCodeLine(line string) string {
	if line == "" {
		return line
	}
	if strings.ContainsRune(line, '`') {
		return line
	}
	return "`" + line + "`"
}

var headingRE = regexp.MustCompile(`^(#+)\s+(.*)$`)

func translateLine(line string) string {
	wrapBold := false
	body := line
	if m := headingRE.FindStringSubmatch(line); m != nil {
		wrapBold = true
		body = m[2]
	}
	body = transformInline(body)
	if wrapBold {
		body = "*" + body + "*"
	}
	return body
}

// transformInline walks the line, peeling off backtick code spans (which pass
// through verbatim) and applying the inline transforms to plain segments.
func transformInline(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '`' {
			closeRel := strings.IndexByte(s[i+1:], '`')
			if closeRel < 0 {
				// no closing backtick — emit rest as plain (treat the lone `
				// as ordinary char so the user still sees their content)
				out.WriteString(applyInlineTransforms(s[i:]))
				return out.String()
			}
			end := i + 1 + closeRel + 1
			out.WriteString(s[i:end])
			i = end
			continue
		}
		nextRel := strings.IndexByte(s[i:], '`')
		if nextRel < 0 {
			out.WriteString(applyInlineTransforms(s[i:]))
			return out.String()
		}
		out.WriteString(applyInlineTransforms(s[i : i+nextRel]))
		i += nextRel
	}
	return out.String()
}

// Sentinel chars used to mark bold spans before the italic pass so single-`*`
// matches can't clobber the bold output. NUL bytes never appear in claude
// output (it's UTF-8 text), so they're safe.
const (
	boldOpen  = "\x00B"
	boldClose = "B\x00"
)

var (
	// Bold content allows `*` so nested italics survive (`**a *b* c**`); the
	// italic pass running afterwards will translate the inner span.
	boldRE   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRE = regexp.MustCompile(`\*([^*\n]+?)\*`)
	strikeRE = regexp.MustCompile(`~~([^~\n]+?)~~`)
	linkRE   = regexp.MustCompile(`(^|[^!])\[([^\]\n]+)\]\(([^)\n]+)\)`)
)

func applyInlineTransforms(s string) string {
	// Go's regexp treats $1 followed by alphanumerics as a single variable
	// name ($1B looks up the variable "1B"), so always brace the index.
	s = boldRE.ReplaceAllString(s, boldOpen+"${1}"+boldClose)
	s = italicRE.ReplaceAllString(s, "_${1}_")
	s = strikeRE.ReplaceAllString(s, "~${1}~")
	s = linkRE.ReplaceAllString(s, "${1}${2} (${3})")
	s = strings.ReplaceAll(s, boldOpen, "*")
	s = strings.ReplaceAll(s, boldClose, "*")
	return s
}
