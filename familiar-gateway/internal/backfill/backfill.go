// Package backfill runs the one-shot relationship extraction pass
// over pre-existing memories. It exists as its own package so both
// the admin HTTP handler (which wraps it in a goroutine with progress
// tracking) and the familiar-ctl CLI (which runs it synchronously
// from the terminal) can drive the same loop without either path
// importing the other.
//
// The package is deliberately narrow: one Run function, one Progress
// struct, and three interfaces that hide the concrete memory /
// sidecar / relationship store behind behavioural contracts. Tests
// can therefore exercise the batching, error accounting, and progress
// reporting with in-memory fakes and no database.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"
)

// Item is a single memory the extractor will process. Only the
// fields the backfill loop needs are present; the full MemoryRow is
// deliberately not imported here so the package stays loose.
type Item struct {
	ID       string
	Content  string
	UserID   string
	ScopeTag string // "" = top-level
}

// Source produces the memories the backfill should walk. One call
// returns every eligible row for the given user — the loop does not
// paginate because the expected volume (hundreds to low thousands)
// fits comfortably in memory and keeps progress reporting trivial.
type Source interface {
	ListForBackfill(ctx context.Context, userID string) ([]Item, error)
}

// Extractor mines entity-relationship triples from a batch of facts.
// Implementations are typically the sidecar client, but tests
// substitute an in-memory fake.
type Extractor interface {
	ExtractRelationshipsFromFacts(ctx context.Context, facts []string) ([]sidecar.ExtractedRelationship, error)
}

// Sink persists the extracted triples, adding only those not already
// stored and reporting how many it added. PgRelationshipStore
// implements this; tests use a capturing fake.
type Sink interface {
	InsertRelationshipsIfAbsent(ctx context.Context, rels []memory.Relationship) (int, error)
}

// Deps bundles the three collaborators the Run loop needs. Kept as a
// single struct so the signature stays short and additions do not
// force every call site to update.
type Deps struct {
	Source    Source
	Extractor Extractor
	Sink      Sink
}

// Options controls a single Run invocation. UserID is required: the
// scan is one user's memories. BatchSize defaults to 15 when zero —
// the same value the spec landed on after empirical sidecar testing.
type Options struct {
	UserID    string
	BatchSize int
}

// ErrNoUser is a run without a user. Memories always have an owner
// now, so the old "empty user = global rows" scan matched nothing and
// reported a successful run of zero rows.
var ErrNoUser = errors.New("backfill: user_id is required")

// Progress is the observable state of a backfill run. It is returned
// from Run, and Run also calls onProgress with the same struct after
// every batch so a wrapping goroutine can publish intermediate state
// to a polling client.
type Progress struct {
	Total      int       `json:"total"`
	Processed  int       `json:"processed"`
	Extracted  int       `json:"extracted"`
	Errors     int       `json:"errors"`
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

// Run is the synchronous backfill loop. It lists the user's eligible
// memories, splits them into batches of Options.BatchSize within one
// scope (so each batch's triples carry its facts' scope tag: an
// isolated shard's triples stay hidden from top-level recall, a wiki
// page's stay in its book), and hands each batch to the extractor.
//
// Provenance: the extractor returns a batch's triples without saying
// which fact each came from, so a triple is attributed to the one fact
// that names both its ends (else the one naming its subject), and to
// none when that's ambiguous. Every triple used to be attributed to
// the batch's first fact.
//
// Only missing triples are added: a re-run doesn't reset triples the
// user re-weighted or re-pointed in the graph editor.
//
// Errors inside a batch are counted in Progress.Errors; the loop
// continues so one flaky batch does not abort the whole run. Only a
// cancelled context terminates the loop early. onProgress is invoked
// after every batch with the current snapshot. It may be nil.
func Run(ctx context.Context, deps Deps, opts Options, onProgress func(Progress)) (Progress, error) {
	if opts.UserID == "" {
		return Progress{}, ErrNoUser
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 15
	}

	items, err := deps.Source.ListForBackfill(ctx, opts.UserID)
	if err != nil {
		return Progress{}, fmt.Errorf("list memories: %w", err)
	}

	prog := Progress{
		Total:     len(items),
		Running:   true,
		StartedAt: time.Now(),
	}
	if onProgress != nil {
		onProgress(prog)
	}

	for _, batch := range scopeBatches(items, batchSize) {
		if err := ctx.Err(); err != nil {
			prog.Running = false
			prog.FinishedAt = time.Now()
			prog.LastError = err.Error()
			if onProgress != nil {
				onProgress(prog)
			}
			return prog, err
		}

		facts := make([]string, 0, len(batch))
		for _, it := range batch {
			facts = append(facts, it.Content)
		}

		triples, extractErr := deps.Extractor.ExtractRelationshipsFromFacts(ctx, facts)
		if extractErr != nil {
			prog.Errors++
			prog.LastError = extractErr.Error()
			prog.Processed += len(batch)
			if onProgress != nil {
				onProgress(prog)
			}
			continue
		}

		if len(triples) > 0 {
			rels := make([]memory.Relationship, 0, len(triples))
			for _, t := range triples {
				rels = append(rels, memory.Relationship{
					Subject:    t.Subject,
					Predicate:  t.Predicate,
					Object:     t.Object,
					UserID:     opts.UserID,
					ScopeTag:   batch[0].ScopeTag,
					SourceFact: sourceOf(t, batch),
					Confidence: 0.9,
				})
			}
			if n, err := deps.Sink.InsertRelationshipsIfAbsent(ctx, rels); err != nil {
				prog.Errors++
				prog.LastError = err.Error()
			} else {
				prog.Extracted += n
			}
		}

		prog.Processed += len(batch)
		if onProgress != nil {
			onProgress(prog)
		}
	}

	prog.Running = false
	prog.FinishedAt = time.Now()
	if onProgress != nil {
		onProgress(prog)
	}
	return prog, nil
}

// scopeBatches splits items into batches of at most size, each within
// one scope tag, keeping the listing's order within a scope.
func scopeBatches(items []Item, size int) [][]Item {
	var order []string
	byScope := map[string][]Item{}
	for _, it := range items {
		if _, ok := byScope[it.ScopeTag]; !ok {
			order = append(order, it.ScopeTag)
		}
		byScope[it.ScopeTag] = append(byScope[it.ScopeTag], it)
	}
	var out [][]Item
	for _, tag := range order {
		group := byScope[tag]
		for start := 0; start < len(group); start += size {
			end := min(start+size, len(group))
			out = append(out, group[start:end])
		}
	}
	return out
}

// sourceOf is the id of the fact a triple came from: the one fact in
// the batch naming both its subject and object, else the one naming
// its subject, else "" (unknown beats wrong).
func sourceOf(t sidecar.ExtractedRelationship, batch []Item) string {
	subj := strings.ToLower(strings.TrimSpace(t.Subject))
	obj := strings.ToLower(strings.TrimSpace(t.Object))
	match := func(needBoth bool) string {
		found := ""
		for _, it := range batch {
			c := strings.ToLower(it.Content)
			if subj == "" || !strings.Contains(c, subj) || (needBoth && !strings.Contains(c, obj)) {
				continue
			}
			if found != "" {
				return "" // ambiguous
			}
			found = it.ID
		}
		return found
	}
	if id := match(true); id != "" {
		return id
	}
	return match(false)
}
