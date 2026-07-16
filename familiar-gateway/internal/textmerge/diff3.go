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
	mMatch := matchMap(b, m) // base idx -> mine idx
	tMatch := matchMap(b, t) // base idx -> theirs idx

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
			// so we keep both rather than clobber. Deterministic order
			// (mine then theirs); for a list the order doesn't matter.
			out = append(out, mReg...)
			out = append(out, tReg...)
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

// matchMap runs an LCS over (base, other) and returns base-index ->
// other-index for every line in the longest common subsequence. Those
// are the base lines that appear, in order, unchanged in `other`.
func matchMap(base, other []string) map[int]int {
	pairs := lcs(base, other)
	out := make(map[int]int, len(pairs))
	for _, p := range pairs {
		out[p[0]] = p[1]
	}
	return out
}

// lcs returns the index pairs (i in a, j in b) of one longest common
// subsequence of the two line slices, in increasing order. O(n*m) —
// fine for wiki pages (tens to low hundreds of lines).
func lcs(a, b []string) [][2]int {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
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
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
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
