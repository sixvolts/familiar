// Package textmerge implements a line-based three-way merge (diff3),
// used to reconcile concurrent wiki-page edits without clobbering.
//
// The shared-list case it exists for: base = "milk", one writer saves
// "milk\neggs", another saves "milk\nbread". Each changed a DIFFERENT
// line relative to the base, so both changes can be kept — the merge
// produces "milk\neggs\nbread" with no conflict. Only when two writers
// change the SAME region differently is a conflict reported, and the
// caller falls back to a manual keep-mine/take-theirs choice.
package textmerge

import "strings"

// Merge performs a line-based three-way merge of `mine` and `theirs`
// against their common ancestor `base`. It returns the merged text and
// whether a conflict occurred (both sides changed the same region in
// different ways). On conflict the merged string is empty and the
// caller must resolve manually — this package intentionally does NOT
// emit conflict markers, because the wiki's fallback is a whole-version
// choice, not inline resolution.
//
// Line splitting/joining round-trips exactly, so a document that only
// differs by disjoint line insertions/deletions/edits merges cleanly
// and byte-for-byte.
func Merge(base, mine, theirs string) (string, bool) {
	// Fast paths: if one side didn't change, take the other verbatim.
	if base == mine {
		return theirs, false
	}
	if base == theirs {
		return mine, false
	}
	if mine == theirs {
		return mine, false
	}

	b := splitLines(base)
	m := splitLines(mine)
	t := splitLines(theirs)

	// Stable anchors: base line indices that survive UNCHANGED in both
	// mine and theirs (matched by the base↔mine and base↔theirs LCS).
	// Between consecutive anchors lies one changed region per side.
	mMatch, okM := matchMap(b, m) // base idx -> mine idx
	tMatch, okT := matchMap(b, t) // base idx -> theirs idx
	if !okM || !okT {
		return "", true // too much changed to diff: treat as a conflict
	}

	var out []string
	prevB, prevM, prevT := 0, 0, 0
	emitRegion := func(bhi, mhi, thi int) bool {
		bReg := b[prevB:bhi]
		mReg := m[prevM:mhi]
		tReg := t[prevT:thi]
		switch {
		case equal(mReg, bReg):
			out = append(out, tReg...) // mine unchanged here -> take theirs
		case equal(tReg, bReg):
			out = append(out, mReg...) // theirs unchanged here -> take mine
		case equal(mReg, tReg):
			out = append(out, mReg...) // both made the same change
		case len(bReg) == 0:
			// Pure insertions on BOTH sides at the same anchor —
			// nothing from base was changed or removed here. Standard
			// diff3 calls this a conflict, but for a shared list it's
			// the common case (two people each add an item at the end),
			// so we keep both rather than clobber: their union, each
			// line both inserted kept once. Appending one after the other
			// doubled such lines: a save retried after its response was
			// lost merged its own earlier "eggs" in again.
			u, ok := unionLines(mReg, tReg)
			if !ok {
				return false
			}
			out = append(out, u...)
		default:
			// Both sides changed or removed the SAME base line(s)
			// differently — a genuine conflict, resolve manually.
			return false
		}
		return true
	}

	for bi := 0; bi < len(b); bi++ {
		mi, okM := mMatch[bi]
		ti, okT := tMatch[bi]
		if !okM || !okT {
			continue // not a shared-stable line; part of a changed region
		}
		// Resolve the region preceding this anchor.
		if !emitRegion(bi, mi, ti) {
			return "", true
		}
		out = append(out, b[bi]) // the anchor line itself
		prevB, prevM, prevT = bi+1, mi+1, ti+1
	}
	// Trailing region after the last anchor.
	if !emitRegion(len(b), len(m), len(t)) {
		return "", true
	}

	return joinLines(out), false
}

// unionLines merges two insertions made at the same place: the lines
// both contain (their LCS) once, in order, and between them each
// side's own lines, mine then theirs. False when they're too large to
// diff.
func unionLines(m, t []string) ([]string, bool) {
	pairs, ok := lcs(m, t)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(m)+len(t)-len(pairs))
	pm, pt := 0, 0
	for _, p := range pairs {
		out = append(out, m[pm:p[0]]...)
		out = append(out, t[pt:p[1]]...)
		out = append(out, m[p[0]])
		pm, pt = p[0]+1, p[1]+1
	}
	out = append(out, m[pm:]...)
	out = append(out, t[pt:]...)
	return out, true
}

// matchMap runs an LCS over (base, other) and returns base-index ->
// other-index for every line in the longest common subsequence. Those
// are the base lines that appear, in order, unchanged in `other`.
func matchMap(base, other []string) (map[int]int, bool) {
	pairs, ok := lcs(base, other)
	if !ok {
		return nil, false
	}
	out := make(map[int]int, len(pairs))
	for _, p := range pairs {
		out[p[0]] = p[1]
	}
	return out, true
}

// lcs returns the index pairs (i in a, j in b) of one longest common
// subsequence of the two line slices, in increasing order. O(n*m) —
// fine for wiki pages (tens to low hundreds of lines).
// maxLCSCells caps the dynamic-programming table the LCS may build
// (rows x columns of the region left after trimming the common prefix
// and suffix). The table is O(n*m) memory: a stale save of a page of
// tens of thousands of short lines asked for gigabytes, and one request
// could get the gateway killed for everyone. Past the cap the merge
// gives up, and the save is refused as stale like any other conflict.
// 4M int32 cells is 16MB; two tables per merge.
const maxLCSCells = 4_000_000

// lcs returns the index pairs of a longest common subsequence of a and
// b, and false when the differing region is too large to diff (see
// maxLCSCells). A common prefix and suffix are always part of some
// LCS, so they are matched directly and only the region between them
// goes through the table: an edit anywhere in a large page costs the
// size of the changed region, not the page.
func lcs(a, b []string) ([][2]int, bool) {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}
	mid, ok := lcsTable(a[p:len(a)-q], b[p:len(b)-q])
	if !ok {
		return nil, false
	}
	pairs := make([][2]int, 0, p+len(mid)+q)
	for i := 0; i < p; i++ {
		pairs = append(pairs, [2]int{i, i})
	}
	for _, pr := range mid {
		pairs = append(pairs, [2]int{pr[0] + p, pr[1] + p})
	}
	for i := 0; i < q; i++ {
		pairs = append(pairs, [2]int{len(a) - q + i, len(b) - q + i})
	}
	return pairs, true
}

// lcsTable is the classic O(n*m) LCS over a and b.
func lcsTable(a, b []string) ([][2]int, bool) {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil, true
	}
	if int64(n+1)*int64(m+1) > maxLCSCells {
		return nil, false
	}
	w := m + 1
	dp := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i*w+j] = dp[(i+1)*w+j+1] + 1
			case dp[(i+1)*w+j] >= dp[i*w+j+1]:
				dp[i*w+j] = dp[(i+1)*w+j]
			default:
				dp[i*w+j] = dp[i*w+j+1]
			}
		}
	}
	var pairs [][2]int
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		case dp[(i+1)*w+j] >= dp[i*w+j+1]:
			i++
		default:
			j++
		}
	}
	return pairs, true
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitLines / joinLines round-trip exactly: a trailing newline becomes
// a trailing empty element that Join reproduces, so merged output that
// touches only disjoint lines is byte-identical to a hand merge.
func splitLines(s string) []string { return strings.Split(s, "\n") }
func joinLines(l []string) string  { return strings.Join(l, "\n") }
