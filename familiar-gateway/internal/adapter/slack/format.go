package slack

import (
	"regexp"
	"strings"
)

// toMrkdwn converts the subset of CommonMark that Familiar actually
// emits into Slack's mrkdwn dialect. The transformation is intentionally
// minimal — pulling in a real Markdown parser would be overkill for the
// handful of constructs we need to fix up:
//
//   - # Heading → *Heading*       (Slack has no headings)
//   - --- / *** / ___ rule → a unicode divider line
//   - "- item" / "+ item" → "•  item"
//   - **bold** → *bold*
//   - [text](url) → <url|text>
//   - <html> tags → stripped
//   - backtick `code` and ``` ```code``` fences pass through untouched
//
// Code spans and fenced blocks are carved out first so the bold/link
// transformations don't accidentally rewrite content the user wanted
// shown verbatim. Everything else is a single regex pass.
func toMrkdwn(s string) string {
	if s == "" {
		return s
	}

	// Split into alternating plain / code segments. Fenced blocks win
	// over inline backticks: we peel off ``` ... ``` first, then within
	// the remaining plain runs we peel off `...`.
	segments := splitCodeSegments(s)

	var b strings.Builder
	b.Grow(len(s))
	for _, seg := range segments {
		if seg.code {
			b.WriteString(seg.text)
			continue
		}
		b.WriteString(transformPlain(seg.text))
	}
	return b.String()
}

type codeSeg struct {
	text string
	code bool // true = do not transform
}

// fencePattern matches ``` fenced code blocks (with or without a language tag).
var fencePattern = regexp.MustCompile("(?s)```[^\n]*\n.*?```|```[^`]*```")

// inlineCodePattern matches `inline code` — non-greedy, single backtick delimited.
var inlineCodePattern = regexp.MustCompile("`[^`\n]+`")

// splitCodeSegments carves the input into plain/code runs in one pass
// by first finding fence spans, then inline code inside the gaps.
func splitCodeSegments(s string) []codeSeg {
	var out []codeSeg
	fences := fencePattern.FindAllStringIndex(s, -1)
	cursor := 0
	for _, span := range fences {
		if span[0] > cursor {
			out = append(out, splitInlineCode(s[cursor:span[0]])...)
		}
		out = append(out, codeSeg{text: s[span[0]:span[1]], code: true})
		cursor = span[1]
	}
	if cursor < len(s) {
		out = append(out, splitInlineCode(s[cursor:])...)
	}
	return out
}

func splitInlineCode(s string) []codeSeg {
	var out []codeSeg
	spans := inlineCodePattern.FindAllStringIndex(s, -1)
	cursor := 0
	for _, span := range spans {
		if span[0] > cursor {
			out = append(out, codeSeg{text: s[cursor:span[0]]})
		}
		out = append(out, codeSeg{text: s[span[0]:span[1]], code: true})
		cursor = span[1]
	}
	if cursor < len(s) {
		out = append(out, codeSeg{text: s[cursor:]})
	}
	return out
}

// headingPattern matches ATX headings (# … ######) at line start,
// trailing #'s optional. Slack has no heading element — bold is the
// idiomatic stand-in.
var headingPattern = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]+(.+?)[ \t]*#*$`)

// hrPattern matches a horizontal rule on its own line (---, ***, ___,
// three or more). Slack mrkdwn has no <hr>, so we draw one. RE2 has
// no backreferences, so the three rule characters are spelled out
// rather than captured-and-repeated.
var hrPattern = regexp.MustCompile(`(?m)^[ \t]*(?:-{3,}|\*{3,}|_{3,})[ \t]*$`)

// bulletPattern matches "- " / "+ " list markers at line start
// (indented or not). Restricted to - and + so it never collides with
// **bold** / *italic* starting a line. Captures the indent.
var bulletPattern = regexp.MustCompile(`(?m)^([ \t]*)[-+][ \t]+`)

const mrkdwnRule = "──────────"

// boldPattern matches **bold** — non-greedy so adjacent bolds don't merge.
var boldPattern = regexp.MustCompile(`\*\*([^*]+)\*\*`)

// linkPattern matches [text](url) — url has no whitespace or paren.
var linkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)

// htmlTagPattern matches simple HTML tags for stripping.
var htmlTagPattern = regexp.MustCompile(`<(/?[a-zA-Z][a-zA-Z0-9]*)\b[^>]*>`)

func transformPlain(s string) string {
	// Order matters:
	//  1. strip HTML before link rewriting so generated <url|text>
	//     isn't re-stripped as a tag;
	//  2. headings/rules/bullets are line-anchored and run before the
	//     inline bold pass — heading text may itself contain **bold**,
	//     which the bold pass then handles;
	//  3. the hr pattern (---) must run before bullets so a "---" line
	//     isn't seen as a "-" bullet.
	s = htmlTagPattern.ReplaceAllString(s, "")
	s = hrPattern.ReplaceAllString(s, mrkdwnRule)
	s = headingPattern.ReplaceAllString(s, "*$1*")
	s = bulletPattern.ReplaceAllString(s, "$1•  ")
	s = boldPattern.ReplaceAllString(s, "*$1*")
	s = linkPattern.ReplaceAllString(s, "<$2|$1>")
	return s
}
