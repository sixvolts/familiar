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
	"fmt"
	"time"

	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"
)

// Item is a single memory the extractor will process. Only the
// fields the backfill loop needs are present; the full MemoryRow is
// deliberately not imported here so the package stays loose.
type Item struct {
	ID      string
	Content string
	UserID  string
}

// Source produces the memories the backfill should walk. One call
// returns every eligible row for the given user scope — the loop
// does not paginate because the expected volume (hundreds to low
// thousands) fits comfortably in memory and keeps progress reporting
// trivial.
type Source interface {
	ListForBackfill(ctx context.Context, userID string) ([]Item, error)
}

// Extractor mines entity-relationship triples from a batch of facts.
// Implementations are typically the sidecar client, but tests
// substitute an in-memory fake.
type Extractor interface {
	ExtractRelationshipsFromFacts(ctx context.Context, facts []string) ([]sidecar.ExtractedRelationship, error)
}

// Sink persists the extracted triples. PgRelationshipStore implements
// this directly; tests use a capturing fake.
type Sink interface {
	UpsertRelationships(ctx context.Context, rels []memory.Relationship) error
}

// Deps bundles the three collaborators the Run loop needs. Kept as a
// single struct so the signature stays short and additions do not
// force every call site to update.
type Deps struct {
	Source    Source
	Extractor Extractor
	Sink      Sink
}

// Options controls a single Run invocation. UserID scopes the scan
// (empty = global rows only). BatchSize defaults to 15 when zero —
// the same value the spec landed on after empirical sidecar testing.
type Options struct {
	UserID    string
	BatchSize int
}

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

// Run is the synchronous backfill loop. It lists every eligible
// memory, splits the slice into batches of Options.BatchSize, and
// hands each batch to the extractor. Extracted triples are converted
// to memory.Relationship rows (anchored at the first memory in the
// batch for provenance) and passed to the sink in one upsert per
// batch. Errors inside a batch are counted in Progress.Errors; the
// loop continues so one flaky batch does not abort the whole run.
// Only a cancelled context terminates the loop early.
//
// onProgress is invoked after every batch with the current snapshot.
// It may be nil.
func Run(ctx context.Context, deps Deps, opts Options, onProgress func(Progress)) (Progress, error) {
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

	for start := 0; start < len(items); start += batchSize {
		if err := ctx.Err(); err != nil {
			prog.Running = false
			prog.FinishedAt = time.Now()
			prog.LastError = err.Error()
			if onProgress != nil {
				onProgress(prog)
			}
			return prog, err
		}

		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]

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
					UserID:     batch[0].UserID,
					SourceFact: batch[0].ID,
					Confidence: 0.9,
				})
			}
			if err := deps.Sink.UpsertRelationships(ctx, rels); err != nil {
				prog.Errors++
				prog.LastError = err.Error()
			} else {
				prog.Extracted += len(rels)
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
