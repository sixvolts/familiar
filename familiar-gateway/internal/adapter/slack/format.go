package slack

import (
	"regexp"
	"strconv"
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
//   - [text](url) and <https://…> autolinks → <url|text> / <url>
//   - <html> tags → stripped
//   - backtick `code` and ``` ```code``` fences pass through, escaped
//   - &, < and > are escaped everywhere, so text can't form a Slack
//     control sequence (<!channel>, <@U…>): content a tool fetched
//     could make the bot ping a whole channel. Only the links built
//     here keep their <…>.
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
			b.WriteString(slackEscape(seg.text))
			continue
		}
		b.WriteString(transformPlain(seg.text))
	}
	return b.String()
}

// slackEscape escapes the three characters Slack gives meaning to.
func slackEscape(s string) string {
	return slackEscaper.Replace(s)
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

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

// autolinkPattern matches a CommonMark autolink, <scheme://…> or
// <mailto:…>. It has to go before the tag strip, which took
// "<https://x>" for a tag named https and deleted the URL.
var autolinkPattern = regexp.MustCompile(`<((?:https?|mailto):[^\s<>]+)>`)

func transformPlain(s string) string {
	// Order matters:
	//  1. links (markdown and autolinks) become placeholders first, so
	//     the tag strip and the escaping leave them alone; they're
	//     restored as Slack links at the end, their text escaped;
	//  2. strip HTML, then escape what's left of &, < and >;
	//  3. headings/rules/bullets are line-anchored and run before the
	//     inline bold pass — heading text may itself contain **bold**,
	//     which the bold pass then handles;
	//  4. the hr pattern (---) must run before bullets so a "---" line
	//     isn't seen as a "-" bullet.
	var links []string
	hold := func(link string) string {
		links = append(links, link)
		return "\x00" + strconv.Itoa(len(links)-1) + "\x00"
	}
	s = linkPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := linkPattern.FindStringSubmatch(m)
		return hold("<" + slackEscape(sub[2]) + "|" + slackEscape(sub[1]) + ">")
	})
	s = autolinkPattern.ReplaceAllStringFunc(s, func(m string) string {
		return hold("<" + slackEscape(autolinkPattern.FindStringSubmatch(m)[1]) + ">")
	})
	s = htmlTagPattern.ReplaceAllString(s, "")
	s = slackEscape(s)
	s = hrPattern.ReplaceAllString(s, mrkdwnRule)
	s = headingPattern.ReplaceAllString(s, "*$1*")
	s = bulletPattern.ReplaceAllString(s, "$1•  ")
	s = boldPattern.ReplaceAllString(s, "*$1*")
	return placeholderPattern.ReplaceAllStringFunc(s, func(m string) string {
		i, _ := strconv.Atoi(strings.Trim(m, "\x00"))
		return links[i]
	})
}

var placeholderPattern = regexp.MustCompile("\x00[0-9]+\x00")
