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
	strikeRE = regexp.MustCompile(`~~([^~\n]+?)~~`)
	linkRE   = regexp.MustCompile(`(^|[^!])\[([^\]\n]+)\]\(([^)\n]+)\)`)
)

func applyInlineTransforms(s string) string {
	// Go's regexp treats $1 followed by alphanumerics as a single variable
	// name ($1B looks up the variable "1B"), so always brace the index.
	s = boldRE.ReplaceAllString(s, boldOpen+"${1}"+boldClose)
	s = transformItalics(s)
	s = strikeRE.ReplaceAllString(s, "~${1}~")
	s = linkRE.ReplaceAllString(s, "${1}${2} (${3})")
	s = strings.ReplaceAll(s, boldOpen, "*")
	s = strings.ReplaceAll(s, boldClose, "*")
	return s
}

// transformItalics rewrites `*italic*` spans to `_italic_` while leaving
// literal asterisks in prose alone (e.g. `a * b`, `*.go and *.md`,
// `a * b * c`). Go's RE2 has no lookaround/backreferences, so a regex can't
// express CommonMark's flanking rules; a small left-to-right scan is clearer
// and lets us require that a span's delimiters hug non-space content.
//
// A `*` opens a span only when it is left-flanking (immediately followed by a
// non-space, non-`*` char) and closes one only when right-flanking (the run
// preceding it is non-empty and its last char is non-space). The span body may
// not contain `*` or whitespace adjacent to either delimiter, which is what
// distinguishes real emphasis from stray asterisks separated by spaces.
//
// Bold sentinels (\x00B / B\x00) have already replaced bold `**…**`, so any
// `*` reaching here is a genuine single-asterisk candidate; this keeps nested
// italics inside bold (`**a *b* c**`) composing correctly.
func transformItalics(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '*' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Candidate opener: must be left-flanking — next char exists and is
		// neither whitespace nor another '*'.
		if i+1 >= len(s) || isSpaceByte(s[i+1]) || s[i+1] == '*' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Scan for a right-flanking closer on the same logical run: the char
		// just before the closing '*' must be non-space, and the body must not
		// contain another '*'.
		closed := -1
		for j := i + 1; j < len(s); j++ {
			if s[j] == '*' {
				if !isSpaceByte(s[j-1]) {
					closed = j
				}
				// First '*' encountered ends the candidate body either way:
				// nested single asterisks aren't valid emphasis here.
				break
			}
		}
		if closed < 0 {
			out.WriteByte(s[i])
			i++
			continue
		}
		out.WriteByte('_')
		out.WriteString(s[i+1 : closed])
		out.WriteByte('_')
		i = closed + 1
	}
	return out.String()
}

// isSpaceByte reports whether b is an ASCII space, tab, or newline — the only
// whitespace the line-oriented translator needs to flank against.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
