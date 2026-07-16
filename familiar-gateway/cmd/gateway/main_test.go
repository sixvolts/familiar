package main

import (
	"net/url"
	"strings"
	"testing"
)

// researchNoteLink builds the deep-path note link. It must produce a
// `#note/<book>/<page>` href whose parts round-trip through the
// frontend's decodeURIComponent — in particular the personal book
// slug's colon must be percent-encoded — and must not let a bracketed
// title break the link markdown.
func TestResearchNoteLink(t *testing.T) {
	got := researchNoteLink("personal:operator", "research-optane", "Research: Optane [draft]")

	// Href present with encoded colon (matches encodeURIComponent).
	wantHref := "#note/" + url.QueryEscape("personal:operator") + "/research-optane"
	if !strings.Contains(got, "("+wantHref+")") {
		t.Errorf("link href missing/wrong.\n got: %s\nwant href: %s", got, wantHref)
	}
	if !strings.Contains(got, "personal%3Aoperator") {
		t.Errorf("book slug colon not percent-encoded: %s", got)
	}
	// Brackets stripped from the label so the markdown link can't break.
	label := got[strings.Index(got, "Open ")+len("Open ") : strings.Index(got, " →")]
	if strings.ContainsAny(label, "[]") {
		t.Errorf("label still contains brackets: %q", label)
	}

	// Empty title degrades to a generic label, still a valid link.
	if g := researchNoteLink("personal:a", "p", ""); !strings.Contains(g, "the note") || !strings.Contains(g, "#note/") {
		t.Errorf("empty-title link malformed: %s", g)
	}
}
