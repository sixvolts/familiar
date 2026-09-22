package search

// Covers the fair-share budget split. The greedy version this replaced let a
// single long source consume the whole budget and drop the rest — measured
// against the live Brave endpoint, one source returned 42 snippets (~5.5k
// chars), which on a 5-source request meant four sources vanished.

import (
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/brave"
)

func TestFormatPageRead_EverySourceSurvivesAHugeOne(t *testing.T) {
	sources := []brave.ContextSource{
		{Title: "Huge", URL: "https://huge.example",
			Snippets: []string{strings.Repeat("x", 9000)}},
		{Title: "Small A", URL: "https://a.example", Snippets: []string{"alpha body"}},
		{Title: "Small B", URL: "https://b.example", Snippets: []string{"beta body"}},
		{Title: "Small C", URL: "https://c.example", Snippets: []string{"gamma body"}},
	}
	out := formatPageRead(sources, 3000)

	for _, want := range []string{
		"https://huge.example", "https://a.example",
		"https://b.example", "https://c.example",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("source %s was dropped — fair-share should keep every URL", want)
		}
	}
	// The short sources are well under their share, so they must appear whole.
	for _, want := range []string{"alpha body", "beta body", "gamma body"} {
		if !strings.Contains(out, want) {
			t.Errorf("short source body %q missing; it fits in its share", want)
		}
	}
	if !strings.Contains(out, "[trimmed") {
		t.Error("the huge source should be marked trimmed")
	}
	// Budget is a cap, not a target; allow the reserved-header overhead.
	if len(out) > 3600 {
		t.Errorf("output %d chars overshoots the 3000 budget too far", len(out))
	}
}

func TestFormatPageRead_ShortSourcesDonateSpare(t *testing.T) {
	// One long source plus three tiny ones: the long one should receive the
	// spare the tiny ones did not use, so it gets far more than budget/4.
	sources := []brave.ContextSource{
		{Title: "Long", URL: "https://l.example",
			Snippets: []string{strings.Repeat("y", 4000)}},
		{Title: "T1", URL: "https://1.example", Snippets: []string{"hi"}},
		{Title: "T2", URL: "https://2.example", Snippets: []string{"hi"}},
		{Title: "T3", URL: "https://3.example", Snippets: []string{"hi"}},
	}
	out := formatPageRead(sources, 2400)

	ys := strings.Count(out, "y")
	quarter := 2400 / 4
	if ys <= quarter {
		t.Errorf("long source got %d chars; should exceed its %d floor via redistribution", ys, quarter)
	}
}

func TestFormatPageRead_AllFitNoTrimNotice(t *testing.T) {
	sources := []brave.ContextSource{
		{Title: "A", URL: "https://a.example", Snippets: []string{"one"}},
		{Title: "B", URL: "https://b.example", Snippets: []string{"two"}},
	}
	out := formatPageRead(sources, 6000)
	if strings.Contains(out, "[trimmed") {
		t.Errorf("nothing should be trimmed when everything fits:\n%s", out)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("both bodies should be present:\n%s", out)
	}
}
