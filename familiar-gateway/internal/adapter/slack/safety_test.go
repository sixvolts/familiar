package slack

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// Text can't form Slack control sequences: a fetched page telling the
// model to print "<!channel>" made the bot ping the channel. Autolinks
// survive (the tag strip deleted them); the links the converter builds
// keep their markup.
func TestToMrkdwn_EscapesAndKeepsLinks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"See <https://example.com/docs> for details", "See <https://example.com/docs> for details"},
		{"Hey <!channel> and <@U12345>", "Hey &lt;!channel&gt; and &lt;@U12345&gt;"},
		{"Tom & Jerry <b>bold</b>", "Tom &amp; Jerry bold"},
		{"[the docs](https://x.test/a?b=1&c=2)", "<https://x.test/a?b=1&amp;c=2|the docs>"},
		{"code: `<!here>`", "code: `&lt;!here&gt;`"},
		{"**bold** <mailto:a@b.test>", "*bold* <mailto:a@b.test>"},
	}
	for _, c := range cases {
		if got := toMrkdwn(c.in); got != c.want {
			t.Errorf("toMrkdwn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Splitting never halves a character or a link, prefers spaces, and
// keeps a code fence rendering as code on both sides.
func TestSplitMessage_RunesLinksAndFences(t *testing.T) {
	// No newline or space: the cut lands on a rune boundary.
	s := strings.Repeat("a", 9) + "é" + strings.Repeat("b", 10)
	for _, c := range splitMessage(s, 10) {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %q is not valid UTF-8", c)
		}
	}
	// A space before the limit wins over a raw cut.
	chunks := splitMessage("hello world again", 13)
	if chunks[0] != "hello world " {
		t.Errorf("chunk[0] = %q, want a cut at the space", chunks[0])
	}
	// Not inside a link (one that fits in a chunk).
	chunks = splitMessage("aaaaaaaaaa<https://x.test|t>tail", 20)
	for _, c := range chunks {
		if strings.Count(c, "<") != strings.Count(c, ">") {
			t.Errorf("a link was cut: %q", chunks)
		}
	}
	// A fence cut in two is closed and reopened.
	code := "```\n" + strings.Repeat("line of code\n", 20) + "```"
	chunks = splitMessage(code, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if strings.Count(c, "```")%2 != 0 {
			t.Errorf("chunk leaves a fence open: %q", c)
		}
		if len(c) > 100 {
			t.Errorf("chunk of %d bytes, over the limit", len(c))
		}
	}
}

// A long proactive post goes as several posts: Slack truncates text
// past 40,000 characters, and nothing split it.
func TestSendProactive_SplitsLongPosts(t *testing.T) {
	api := &fakeSlackAPI{membersPage: [][]string{{}}}
	srv := api.server(t)
	s, err := NewSender("xoxb-test", srv.URL+"/api/")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("A research digest paragraph.\n", 3000) // ~87KB
	if err := s.SendProactive(context.Background(), "C_TEAM", long); err != nil {
		t.Fatal(err)
	}
	if len(api.texts) < 3 {
		t.Fatalf("posted %d messages, want the digest split", len(api.texts))
	}
	total := 0
	for _, txt := range api.texts {
		if len(txt) > proactiveMaxLen {
			t.Errorf("a post of %d bytes", len(txt))
		}
		total += len(txt)
	}
	if total < len(long)-10 {
		t.Errorf("posted %d of %d bytes", total, len(long))
	}
}
