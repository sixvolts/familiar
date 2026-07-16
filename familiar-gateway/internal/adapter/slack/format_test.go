package slack

import "testing"

func TestToMrkdwn_Bold(t *testing.T) {
	got := toMrkdwn("this is **bold** text")
	want := "this is *bold* text"
	if got != want {
		t.Errorf("bold: got %q want %q", got, want)
	}
}

func TestToMrkdwn_MultipleBold(t *testing.T) {
	got := toMrkdwn("**one** and **two**")
	want := "*one* and *two*"
	if got != want {
		t.Errorf("multi bold: got %q want %q", got, want)
	}
}

func TestToMrkdwn_Link(t *testing.T) {
	got := toMrkdwn("see [the docs](https://example.com/x) now")
	want := "see <https://example.com/x|the docs> now"
	if got != want {
		t.Errorf("link: got %q want %q", got, want)
	}
}

func TestToMrkdwn_StripHTML(t *testing.T) {
	got := toMrkdwn("<p>hi <b>there</b></p>")
	want := "hi there"
	if got != want {
		t.Errorf("html: got %q want %q", got, want)
	}
}

func TestToMrkdwn_InlineCodePassthrough(t *testing.T) {
	// Bold markers inside inline code must NOT be rewritten.
	got := toMrkdwn("use `**bold**` to make text bold")
	want := "use `**bold**` to make text bold"
	if got != want {
		t.Errorf("inline code: got %q want %q", got, want)
	}
}

func TestToMrkdwn_FencedCodePassthrough(t *testing.T) {
	in := "before\n```go\n**keep this**\n[as is](http://x)\n```\nafter **bold**"
	want := "before\n```go\n**keep this**\n[as is](http://x)\n```\nafter *bold*"
	if got := toMrkdwn(in); got != want {
		t.Errorf("fenced: got %q want %q", got, want)
	}
}

func TestToMrkdwn_Empty(t *testing.T) {
	if got := toMrkdwn(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

func TestToMrkdwn_NoTransforms(t *testing.T) {
	in := "plain text with no markup"
	if got := toMrkdwn(in); got != in {
		t.Errorf("plain: got %q want %q", got, in)
	}
}

func TestToMrkdwn_Headings(t *testing.T) {
	cases := map[string]string{
		"# Title":         "*Title*",
		"## Section":      "*Section*",
		"### Sub  ":       "*Sub*",
		"## Closed ##":    "*Closed*",
		"###### Deep":     "*Deep*",
		"not # a heading": "not # a heading",
	}
	for in, want := range cases {
		if got := toMrkdwn(in); got != want {
			t.Errorf("heading %q: got %q want %q", in, got, want)
		}
	}
}

func TestToMrkdwn_HorizontalRule(t *testing.T) {
	for _, in := range []string{"---", "***", "___", "  ----  "} {
		got := toMrkdwn("a\n" + in + "\nb")
		want := "a\n" + mrkdwnRule + "\nb"
		if got != want {
			t.Errorf("hr %q: got %q want %q", in, got, want)
		}
	}
}

func TestToMrkdwn_Bullets(t *testing.T) {
	in := "- first\n- second\n  - nested\n+ plus"
	want := "•  first\n•  second\n  •  nested\n•  plus"
	if got := toMrkdwn(in); got != want {
		t.Errorf("bullets: got %q want %q", got, want)
	}
}

func TestToMrkdwn_BulletNotBold(t *testing.T) {
	// A line starting with **bold** must NOT be eaten by the bullet
	// rule (it only matches - and +), and bold still converts.
	got := toMrkdwn("**Heads up** read this")
	want := "*Heads up* read this"
	if got != want {
		t.Errorf("bold-at-line-start: got %q want %q", got, want)
	}
}

func TestToMrkdwn_HrNotBullet(t *testing.T) {
	// "---" must become a rule, never a "-" bullet.
	if got := toMrkdwn("---"); got != mrkdwnRule {
		t.Errorf("hr-vs-bullet: got %q want %q", got, mrkdwnRule)
	}
}

func TestToMrkdwn_HeadingInCodeUntouched(t *testing.T) {
	in := "```\n# not a heading\n- not a bullet\n```"
	if got := toMrkdwn(in); got != in {
		t.Errorf("code passthrough: got %q want %q", got, in)
	}
}
