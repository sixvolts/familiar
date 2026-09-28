package textmerge

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		name             string
		base, mine, thrs string
		want             string
		wantConflict     bool
	}{
		{
			name: "the grocery list — each adds a different item",
			base: "- milk",
			mine: "- milk\n- eggs",
			thrs: "- milk\n- bread",
			want: "- milk\n- eggs\n- bread",
		},
		{
			name: "one adds at top, other adds at bottom",
			base: "- milk\n- eggs",
			mine: "- butter\n- milk\n- eggs",
			thrs: "- milk\n- eggs\n- bread",
			want: "- butter\n- milk\n- eggs\n- bread",
		},
		{
			name: "one edits a line, other adds an unrelated line",
			base: "- milk\n- eggs",
			mine: "- oat milk\n- eggs",      // changed line 1
			thrs: "- milk\n- eggs\n- bread", // added line 3
			want: "- oat milk\n- eggs\n- bread",
		},
		{
			name: "one removes a line, other adds a different line",
			base: "- milk\n- eggs\n- soda",
			mine: "- milk\n- soda",                  // removed eggs
			thrs: "- milk\n- eggs\n- soda\n- bread", // added bread
			want: "- milk\n- soda\n- bread",
		},
		{
			name:         "both change the SAME line differently — conflict",
			base:         "- milk",
			mine:         "- oat milk",
			thrs:         "- soy milk",
			wantConflict: true,
		},
		{
			name:         "both insert a different line at the same empty spot — conflict",
			base:         "",
			mine:         "hello",
			thrs:         "world",
			wantConflict: true,
		},
		{
			name: "both make the identical change — no conflict, one copy",
			base: "- milk",
			mine: "- milk\n- eggs",
			thrs: "- milk\n- eggs",
			want: "- milk\n- eggs",
		},
		{
			name: "mine unchanged from base — take theirs",
			base: "- milk",
			mine: "- milk",
			thrs: "- milk\n- eggs",
			want: "- milk\n- eggs",
		},
		{
			name: "theirs unchanged from base — take mine",
			base: "- milk",
			mine: "- milk\n- eggs",
			thrs: "- milk",
			want: "- milk\n- eggs",
		},
		{
			name: "disjoint edits to lines far apart",
			base: "a\nb\nc\nd\ne",
			mine: "a\nB\nc\nd\ne", // changed b
			thrs: "a\nb\nc\nd\nE", // changed e
			want: "a\nB\nc\nd\nE",
		},
		{
			name: "trailing newline preserved (exact round-trip)",
			base: "- milk\n",
			mine: "- milk\n- eggs\n",
			thrs: "- milk\n- bread\n",
			want: "- milk\n- eggs\n- bread\n",
		},
		{
			name: "identical documents",
			base: "x\ny",
			mine: "x\ny",
			thrs: "x\ny",
			want: "x\ny",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, conflict := Merge(c.base, c.mine, c.thrs)
			if conflict != c.wantConflict {
				t.Fatalf("conflict = %v, want %v (got merged=%q)", conflict, c.wantConflict, got)
			}
			if !c.wantConflict && got != c.want {
				t.Errorf("merged =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

// A clean merge must be symmetric: swapping mine/theirs yields the same
// set of lines (order may differ where both insert at the same anchor,
// but the disjoint-line cases the wiki cares about are stable).
func TestMerge_SymmetryOnDisjointEdits(t *testing.T) {
	base := "- milk\n- eggs"
	a := "- milk\n- eggs\n- bread"
	b := "- butter\n- milk\n- eggs"
	m1, c1 := Merge(base, a, b)
	m2, c2 := Merge(base, b, a)
	if c1 || c2 {
		t.Fatalf("unexpected conflict: %v %v", c1, c2)
	}
	if m1 != m2 {
		t.Errorf("asymmetric merge:\n%q\nvs\n%q", m1, m2)
	}
}

func numberedLines(n int, edit func(i int, line string) string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		line := fmt.Sprintf("line %d", i)
		if edit != nil {
			line = edit(i, line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Edits at opposite ends of a large page still merge, cheaply: only the
// changed region between the common prefix and suffix is diffed.
func TestMerge_LargePageFarApartEdits(t *testing.T) {
	const n = 40000
	base := numberedLines(n, nil)
	mine := numberedLines(n, func(i int, l string) string {
		if i == 10 {
			return "mine changed the top"
		}
		return l
	})
	theirs := numberedLines(n, func(i int, l string) string {
		if i == n-10 {
			return "theirs changed the bottom"
		}
		return l
	})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, conflict := Merge(base, mine, theirs)
	runtime.ReadMemStats(&after)
	if conflict {
		t.Fatal("far-apart edits on a large page conflicted")
	}
	if !strings.Contains(got, "mine changed the top") || !strings.Contains(got, "theirs changed the bottom") {
		t.Error("merge lost one of the edits")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("merge allocated %d MB", alloc>>20)
	}
}

// A stale save that changes lines throughout a large page is refused as
// a conflict instead of building an n*m table (gigabytes at this size,
// which got the gateway killed).
func TestMerge_ScatteredRewriteOfLargePageIsAConflict(t *testing.T) {
	const n = 40000
	base := numberedLines(n, nil)
	mine := numberedLines(n, func(i int, l string) string {
		if i%2 == 0 {
			return l + " (mine)"
		}
		return l
	})
	theirs := numberedLines(n, func(i int, l string) string {
		if i%3 == 0 {
			return l + " (theirs)"
		}
		return l
	})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, conflict := Merge(base, mine, theirs)
	runtime.ReadMemStats(&after)
	if !conflict {
		t.Error("a merge too large to diff was not refused")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("refusing the merge allocated %d MB", alloc>>20)
	}
}

// Both sides inserting overlapping lines at one place keeps each line
// once: the retried save, and two people who both added "eggs".
func TestMerge_OverlappingInsertionsNotDuplicated(t *testing.T) {
	cases := []struct{ base, mine, theirs, want string }{
		{"- milk\n", "- milk\n- eggs\n", "- milk\n- eggs\n- bread\n", "- milk\n- eggs\n- bread\n"},
		{"- milk\n", "- milk\n- eggs\n- bread\n", "- milk\n- eggs\n", "- milk\n- eggs\n- bread\n"},
		{"- milk\n", "- milk\n- eggs\n- jam\n", "- milk\n- bread\n- eggs\n", "- milk\n- bread\n- eggs\n- jam\n"},
		// Disjoint insertions still keep both.
		{"- milk\n", "- milk\n- eggs\n", "- milk\n- bread\n", "- milk\n- eggs\n- bread\n"},
	}
	for _, c := range cases {
		got, conflict := Merge(c.base, c.mine, c.theirs)
		if conflict || got != c.want {
			t.Errorf("Merge(%q, %q, %q) = %q, %v; want %q", c.base, c.mine, c.theirs, got, conflict, c.want)
		}
	}
}
