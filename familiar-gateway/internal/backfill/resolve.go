package backfill

import (
	"context"
	"fmt"
	"sort"

	"github.com/familiar/gateway/internal/sidecar"
)

// EntitySource lists every distinct entity name (subjects + objects)
// for a user scope. Kept behind an interface so the resolve loop can
// be driven by either the real PgRelationshipStore or an in-memory
// fake during tests.
type EntitySource interface {
	ListDistinctEntities(ctx context.Context, userID string) (map[string]int, error)
}

// EntityGrouper clusters a batch of noisy entity names. The real
// implementation is *sidecar.Client; tests substitute a canned grouper
// so the resolution logic can be exercised without an LLM.
type EntityGrouper interface {
	GroupEntities(ctx context.Context, names []string) ([]sidecar.EntityGroup, error)
}

// EntityMerger rewrites the graph so every occurrence of one of the
// aliases becomes the canonical name. The PgRelationshipStore.MergeEntity
// method satisfies this interface.
type EntityMerger interface {
	MergeEntity(ctx context.Context, userID string, canonical string, aliases []string) (int64, error)
}

// ResolveDeps bundles the three collaborators the resolve loop needs.
// Kept as a single struct so the call site does not need to update
// every time a dependency is added.
type ResolveDeps struct {
	Source  EntitySource
	Grouper EntityGrouper
	Merger  EntityMerger
}

// ResolveOptions controls a single Resolve invocation. UserID scopes
// the resolution; empty means "global rows only". ChunkSize caps the
// number of entity names sent to the grouper in a single LLM call —
// the prompt scales linearly with input so wide batches blow the
// context window. MinRefs drops entities that appear only once or twice
// from the grouping input because the long tail of single-use names
// is overwhelmingly noise (typos, one-off mentions, predicate objects
// that are not really entities).
type ResolveOptions struct {
	UserID    string
	ChunkSize int
	MinRefs   int
}

// ResolveProgress is the observable state of a resolve run. Mirrors
// the shape of backfill.Progress so the same CLI progress reporting
// pattern works for both.
type ResolveProgress struct {
	TotalEntities  int                   `json:"total_entities"`
	GroupsProposed int                   `json:"groups_proposed"`
	EntitiesMerged int                   `json:"entities_merged"`
	RowsRewritten  int64                 `json:"rows_rewritten"`
	Errors         int                   `json:"errors"`
	LastError      string                `json:"last_error,omitempty"`
	ProposedGroups []sidecar.EntityGroup `json:"proposed_groups,omitempty"`
}

// Resolve is the synchronous entity-resolution loop. It lists every
// distinct entity for the user scope, sends them to the grouper in
// chunks, and applies each proposed merge through the merger. Errors
// inside a chunk are counted in Errors; the loop continues so one
// flaky batch does not abort the whole pass.
//
// onProgress is invoked after every chunk with the current snapshot.
// It may be nil.
func Resolve(ctx context.Context, deps ResolveDeps, opts ResolveOptions, onProgress func(ResolveProgress)) (ResolveProgress, error) {
	chunk := opts.ChunkSize
	if chunk <= 0 {
		chunk = 60
	}
	minRefs := opts.MinRefs
	if minRefs <= 0 {
		minRefs = 1
	}

	entMap, err := deps.Source.ListDistinctEntities(ctx, opts.UserID)
	if err != nil {
		return ResolveProgress{}, fmt.Errorf("list entities: %w", err)
	}

	// Filter by MinRefs and sort descending by refcount so the most
	// referenced (and therefore most impactful to canonicalise) entities
	// land in the first chunk. Ties broken alphabetically for
	// determinism under tests.
	names := make([]string, 0, len(entMap))
	for n, c := range entMap {
		if c >= minRefs {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if entMap[names[i]] != entMap[names[j]] {
			return entMap[names[i]] > entMap[names[j]]
		}
		return names[i] < names[j]
	})

	prog := ResolveProgress{TotalEntities: len(names)}
	if onProgress != nil {
		onProgress(prog)
	}

	for start := 0; start < len(names); start += chunk {
		if err := ctx.Err(); err != nil {
			prog.LastError = err.Error()
			if onProgress != nil {
				onProgress(prog)
			}
			return prog, err
		}
		end := start + chunk
		if end > len(names) {
			end = len(names)
		}
		batch := names[start:end]

		groups, err := deps.Grouper.GroupEntities(ctx, batch)
		if err != nil {
			prog.Errors++
			prog.LastError = err.Error()
			if onProgress != nil {
				onProgress(prog)
			}
			continue
		}

		for _, g := range groups {
			if g.Canonical == "" || len(g.Aliases) == 0 {
				continue
			}
			prog.GroupsProposed++
			prog.ProposedGroups = append(prog.ProposedGroups, g)
			n, err := deps.Merger.MergeEntity(ctx, opts.UserID, g.Canonical, g.Aliases)
			if err != nil {
				prog.Errors++
				prog.LastError = err.Error()
				continue
			}
			prog.EntitiesMerged += len(g.Aliases)
			prog.RowsRewritten += n
		}

		if onProgress != nil {
			onProgress(prog)
		}
	}

	return prog, nil
}
