package sidecar

import "testing"

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Postgres Indexing", "Postgres Indexing"},
		{"quotes stripped", `"Cat Names"`, "Cat Names"},
		{"trailing punctuation", "Quick Chat.", "Quick Chat"},
		{"first line only", "Database Tuning\nsome rambling", "Database Tuning"},
		{"label scaffold stripped", "Title: Network Setup", "Network Setup"},
		{"clamped to three words", "A Very Long Runaway Title", "A Very Long"},
		{"surrounding whitespace", "  Trim Me  ", "Trim Me"},
		{"empty", "", ""},
		{"punctuation only", `".,;"`, ""},
		{"think block stripped", "<think>\nhmm, the topic is postgres\n</think>\n\nPostgres Indexing", "Postgres Indexing"},
		{"think block then blank", "<think>reasoning</think>\n\n\nCat Names", "Cat Names"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanTitle(tc.in); got != tc.want {
				t.Errorf("cleanTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClampForTitle(t *testing.T) {
	if got := clampForTitle("short", 100); got != "short" {
		t.Errorf("under-limit string should pass through, got %q", got)
	}
	long := "abcdefghij"
	if got := clampForTitle(long, 4); len(got) > 4 {
		t.Errorf("clampForTitle(%q,4) = %q — over the byte budget", long, got)
	}
	// A multi-byte rune straddling the cut must not be split into
	// invalid UTF-8.
	if got := clampForTitle("aé", 2); got != "a" {
		t.Errorf("clampForTitle should back off a split rune, got %q", got)
	}
}
