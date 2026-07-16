package notesmigration

// Pure-function tests. The SQL-touching paths (Plan, MigrateUser,
// VerifyUser) need a real Postgres pool and live alongside the
// other admin-package integration tests.

import "testing"

func TestSlugify_BasicShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"  Trimmed  ", "trimmed"},
		{"!!!special???chars!!!", "special-chars"},
		{"", "untitled"},
		{"   ", "untitled"},
		{"already-clean", "already-clean"},
		{"UPPER", "upper"},
		{"underscores_become_dashes", "underscores-become-dashes"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlugify_TruncatesLong(t *testing.T) {
	long := "a-very-long-title-that-keeps-going-and-going-and-going-past-the-cap-by-some-amount"
	got := slugify(long)
	if len(got) > 60 {
		t.Errorf("slugify did not cap length: len=%d, slug=%q", len(got), got)
	}
}

func TestUniqueSlugInSet_NoCollision(t *testing.T) {
	used := map[string]bool{}
	got := uniqueSlugInSet("intro", used)
	if got != "intro" {
		t.Errorf("first use should pass through; got %q", got)
	}
}

func TestUniqueSlugInSet_AppendsSuffix(t *testing.T) {
	used := map[string]bool{"intro": true}
	got := uniqueSlugInSet("intro", used)
	if got != "intro-2" {
		t.Errorf("first collision = %q, want intro-2", got)
	}
	used[got] = true
	got2 := uniqueSlugInSet("intro", used)
	if got2 != "intro-3" {
		t.Errorf("second collision = %q, want intro-3", got2)
	}
}

func TestUniqueSlugInSet_HoleInSequenceIsFilled(t *testing.T) {
	used := map[string]bool{"x": true, "x-3": true}
	// Caller hasn't pre-claimed x-2; we should hand it out.
	got := uniqueSlugInSet("x", used)
	if got != "x-2" {
		t.Errorf("hole-in-sequence = %q, want x-2", got)
	}
}
