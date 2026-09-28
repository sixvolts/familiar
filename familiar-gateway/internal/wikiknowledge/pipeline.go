// Package wikiknowledge runs the async knowledge-ingestion pipeline
// for wiki pages (BOOKS-WIKI-ARCHITECTURE Phase 1 step 6).
//
// On a page save (debounced, one run per page at a time; see PageSaved):
//
//  1. Hand the page body to the sidecar's ExtractFacts (same
//     extraction it uses on conversation turns).
//  2. Only if that succeeded, replace the page's facts with the
//     result in one transaction (Engine.ReplaceSourceFacts), with
//     scope_tag = "book:{id}", source_type = "wiki_page", source_ref =
//     "page:{page_id}". A failed or timed-out extraction leaves the
//     page's existing facts alone. Every current member of the book
//     recalls them (and its triples), whoever saved the page.
//  3. Upsert any extracted entity-relationship triples into
//     relationships, again carrying the book scope_tag.
//  4. For every resolved [[]] outbound link on this page, emit
//     a links_to triple subject="page:{book_slug}/{page_slug}",
//     object="page:{target_book_slug}/{target_page_slug}", same
//     scope_tag. Broken links (target_page_id == nil) are skipped
//     until the target exists.
//
// On page delete:
//
//   - DELETE memory rows for the page. Relationship triples are
//     left in place — they're cross-page and re-affirmed on every
//     other page's save.
//
// Best-effort throughout. Every step logs on failure but never
// rolls back the page write that triggered the run, on the same
// principle as the wiki audit log: ingestion gaps are recoverable
// (re-save), permission breakage isn't.
package wikiknowledge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/safego"
	"github.com/familiar/gateway/internal/sidecar"

	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EngineClient is the slice of engine.Service we depend on. Defined
// here so tests can stub without dragging the real memengine in.
type EngineClient interface {
	ReplaceSourceFacts(ctx context.Context, sessionID, sourceType, sourceRef, scopeTag string, facts []*pb.FactProto) (int64, error)
}

// SidecarClient is the slice of *sidecar.Client we depend on.
// ExtractFactsLarge routes a big document (a research write-up) to a
// bigger model that can hold it in context; it falls back to the small
// extract model when no large route is configured.
type SidecarClient interface {
	ExtractFacts(ctx context.Context, turns []sidecar.Turn) (sidecar.ExtractionResult, error)
	ExtractFactsLarge(ctx context.Context, turns []sidecar.Turn) (sidecar.ExtractionResult, error)
}

// largeExtractChars is the body size above which extraction is routed
// to the large-document model. The small extract model (~8K context)
// overruns on a multi-KB note; a research write-up runs 5–12K chars.
const largeExtractChars = 4000

// MemoryStore is the slice of *memory.PgVectorStore we depend on
// for page-delete cleanup.
type MemoryStore interface {
	DeleteMemoriesBySource(ctx context.Context, sourceType, sourceRef, scopeTag string) (int64, error)
}

// RelationshipStore is the slice of *memory.PgRelationshipStore we
// depend on for triple persistence.
type RelationshipStore interface {
	UpsertRelationships(ctx context.Context, rels []memory.Relationship) error
	ReplacePageLinks(ctx context.Context, subject, userID, scopeTag string, objects []string) error
}

// EmbedFunc computes an embedding for a fact's content. Pipeline
// tolerates a nil return — the engine will store the fact without
// the vector and the next backfill will fill it in.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// Deps bundles the optional collaborators. A nil collaborator is
// fine — the pipeline degrades gracefully (skipping that step
// while still doing the rest).
type Deps struct {
	Engine      EngineClient
	Sidecar     SidecarClient
	MemoryStore MemoryStore
	RelStore    RelationshipStore
	Embedder    EmbedFunc

	// Timeout caps one ingestion run, so a wedged sidecar can't leak a
	// goroutine forever. It has to outlast the slowest extraction: a
	// large page goes to the extract_large route, which takes minutes
	// and first waits for the model's slot. The old 30s cap cancelled
	// every large page's extraction. Zero uses defaultTimeout.
	Timeout time.Duration

	// Debounce is how long a page must go without a save before it is
	// ingested. Editors autosave 500ms after typing stops, and each
	// save used to start its own extraction. Zero uses defaultDebounce.
	Debounce time.Duration
}

const (
	// defaultTimeout is the extract_large ceiling plus a minute for the
	// sidecar's slot gate.
	defaultTimeout  = sidecar.LargeExtractTimeout + time.Minute
	defaultDebounce = 10 * time.Second
)

// Pipeline is the wiki knowledge runner. Construct one per gateway
// process; the wiki store's save hook calls PageSaved and its delete
// hook calls OnPageDeleted.
type Pipeline struct {
	deps Deps

	mu    sync.Mutex
	pages map[string]*pageState // by page id; guarded by mu
}

// pageState tracks one page between saves and ingestion runs.
type pageState struct {
	// gen counts the page's saves and deletes. A run commits only if
	// gen hasn't moved since it read the page, so an older draft's
	// extraction can never land after a newer one's. Guarded by
	// Pipeline.mu, like latest and timer.
	gen    uint64
	latest SaveEvent   // the newest save
	timer  *time.Timer // the pending debounce, nil when none

	run    sync.Mutex // one ingestion run per page at a time
	commit sync.Mutex // a run's check-and-commit vs. the page's delete
}

// New constructs a Pipeline. Returns nil if Deps is empty enough
// that no work could happen.
func New(deps Deps) *Pipeline {
	if deps.Timeout <= 0 {
		deps.Timeout = defaultTimeout
	}
	if deps.Debounce <= 0 {
		deps.Debounce = defaultDebounce
	}
	if deps.Engine == nil && deps.Sidecar == nil && deps.MemoryStore == nil && deps.RelStore == nil {
		return nil
	}
	return &Pipeline{deps: deps, pages: map[string]*pageState{}}
}

// SaveEvent is the payload the wiki store hands the pipeline after
// a CreatePage or UpdatePage tx commits. PageLinks is the outbound
// link snapshot from WikiStore.ListPageLinks (already resolved).
type SaveEvent struct {
	BookID   string
	BookSlug string
	PageID   string
	PageSlug string
	UserID   string
	Title    string
	Content  string
	Links    []admin.PageLink
}

// DeleteEvent is the payload for a soft-delete. We keep relationship
// triples (they're cross-page) and only sweep this page's facts /
// embeddings.
type DeleteEvent struct {
	BookID   string
	BookSlug string
	PageID   string
	PageSlug string
}

// skipBook reports books whose pages are never ingested. Research
// evidence books (slug research:{userID}) hold transient raw web
// scratch that gets reaped, not knowledge worth remembering —
// extracting facts from it would pollute memory with low-value,
// possibly prompt-injected content (RESEARCH-SKILL-SPEC §9) and leave
// facts orphaned when the sweep deletes the page.
func skipBook(bookSlug string) bool {
	return strings.HasPrefix(bookSlug, "research:")
}

// PageSaved schedules ingestion for a saved page and returns at once.
// The run starts once the page has gone Debounce without another save
// and always ingests the newest save. Runs for one page never overlap,
// and a run that a newer save or a delete overtook while it was
// extracting discards its result.
func (p *Pipeline) PageSaved(evt SaveEvent) {
	if p == nil || skipBook(evt.BookSlug) || evt.PageID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.pages[evt.PageID]
	if st == nil {
		st = &pageState{}
		p.pages[evt.PageID] = st
	}
	st.gen++
	st.latest = evt
	if st.timer != nil {
		st.timer.Stop()
	}
	due := st.gen
	st.timer = time.AfterFunc(p.deps.Debounce, func() { p.fire(evt.PageID, st, due) })
}

// fire runs a debounced ingestion. due is the save that armed the
// timer: if a later save re-armed it (Stop can lose that race) or the
// page was deleted, this firing does nothing.
func (p *Pipeline) fire(pageID string, st *pageState, due uint64) {
	defer safego.Recover("wiki knowledge extraction " + pageID)
	p.mu.Lock()
	if p.pages[pageID] != st || st.gen != due {
		p.mu.Unlock()
		return
	}
	st.timer = nil
	p.mu.Unlock()

	st.run.Lock()
	defer st.run.Unlock()

	p.mu.Lock()
	evt, gen, live := st.latest, st.gen, p.pages[pageID] == st
	p.mu.Unlock()
	if live {
		_ = p.ingest(context.Background(), evt, st, gen)
	}

	// Forget an idle page, so the map (and the content in latest)
	// doesn't grow with every page ever saved.
	p.mu.Lock()
	if p.pages[pageID] == st && st.gen == gen && st.timer == nil {
		delete(p.pages, pageID)
	}
	p.mu.Unlock()
}

// current reports whether gen is still the page's newest save.
func (p *Pipeline) current(pageID string, st *pageState, gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pages[pageID] == st && st.gen == gen
}

// OnPageSaved ingests one page synchronously, with no debounce and no
// check for newer saves. PageSaved is the entry point for the save
// hook; this is the direct form.
//
// Step ordering matters:
//  1. Extract BEFORE touching the page's existing facts, and replace
//     them only with a successful result: a sidecar that is down,
//     busy or slow must not leave the page with no facts at all.
//  2. Facts BEFORE relationships, so a relationship that references
//     a fresh fact id (Phase 2 work) finds it.
//  3. Wiki link triples LAST, after the extracted relationships,
//     so a links_to edge to a page that's also mentioned in the
//     content overwrites cleanly.
func (p *Pipeline) OnPageSaved(ctx context.Context, evt SaveEvent) {
	if p == nil || skipBook(evt.BookSlug) {
		return
	}
	_ = p.ingest(ctx, evt, nil, 0)
}

// Outcome is what one ingestion did with a page.
type Outcome int

const (
	// Committed: the page's facts now come from this content (a body too
	// short to extract from committed none).
	Committed Outcome = iota
	// Superseded: a newer save or a delete overtook the run, which
	// changed nothing; the newer save's own run handles the page.
	Superseded
	// Skipped: the page isn't ingested (research evidence, or gone).
	Skipped
	// Failed: extraction or the write failed; the page's existing
	// facts were left as they were.
	Failed
)

// ingest runs one ingestion. With st set, it commits only while gen is
// still the page's newest save (see pageState.gen).
func (p *Pipeline) ingest(ctx context.Context, evt SaveEvent, st *pageState, gen uint64) Outcome {
	ctx, cancel := context.WithTimeout(ctx, p.deps.Timeout)
	defer cancel()

	scopeTag := scopeTagFor(evt.BookID)
	sourceRef := pageSourceRef(evt.PageID)

	// 1. Extract. ExtractFacts takes []Turn — we wrap the page as a
	// single user turn with the title for context. A body too small to
	// be worth the LLM round-trip (an empty page or a one-word note)
	// holds no knowledge, so it replaces the page's facts with none.
	body := strings.TrimSpace(evt.Content)
	var extraction sidecar.ExtractionResult
	extracted := false
	switch {
	case len(body) <= 32:
		extracted = true
	case p.deps.Sidecar != nil:
		turns := []sidecar.Turn{
			{Role: "user", Content: "Page title: " + evt.Title + "\n\n" + body},
		}
		// Large documents (research write-ups) overrun the small extract
		// model — route them to the big-model extract, which falls back
		// to the small one when no large route is configured.
		var err error
		if len(body) > largeExtractChars {
			extraction, err = p.deps.Sidecar.ExtractFactsLarge(ctx, turns)
		} else {
			extraction, err = p.deps.Sidecar.ExtractFacts(ctx, turns)
		}
		if err != nil {
			log.Printf("[wikiknowledge] extract failed for %s (keeping its existing facts): %v", sourceRef, err)
		} else {
			extracted = true
		}
	}
	var facts []*pb.FactProto
	if extracted {
		facts = p.buildFacts(ctx, evt, scopeTag, sourceRef, extraction.Facts)
	}

	// 2. Replace the page's facts, unless a newer save or a delete got
	// here first. commit is held across the check and the write so a
	// delete can't slip in between them.
	if st != nil {
		st.commit.Lock()
		defer st.commit.Unlock()
		if !p.current(evt.PageID, st, gen) {
			log.Printf("[wikiknowledge] discarding superseded run for %s", sourceRef)
			return Superseded
		}
	}
	outcome := Failed
	if extracted && p.deps.Engine != nil && evt.PageID != "" {
		removed, err := p.deps.Engine.ReplaceSourceFacts(ctx, "wiki:"+evt.BookID, "wiki_page", sourceRef, scopeTag, facts)
		if err != nil {
			log.Printf("[wikiknowledge] replacing facts for %s failed (keeping the old ones): %v", sourceRef, err)
		} else {
			log.Printf("[wikiknowledge] %s: %d fact(s), replacing %d", sourceRef, len(facts), removed)
			outcome = Committed
		}
	}
	if extracted {
		p.upsertExtractedRelationships(ctx, evt, scopeTag, extraction.Relationships)
	}

	// 3. Wiki link triples for resolved outbound links.
	p.upsertLinkTriples(ctx, evt, scopeTag)
	return outcome
}

// Reingest re-extracts one page now, for the re-index job, reading its
// content through load (false: the page is gone). It runs under the same
// per-page rules as a save:
//
//   - a page with a save pending or being ingested is left to that run,
//     which has content at least as new (Superseded);
//   - the content is read only after the page is registered here, so a
//     save that lands after the read bumps the page's generation, and
//     this run's result is discarded rather than committed over it.
func (p *Pipeline) Reingest(ctx context.Context, pageID string, load func(context.Context) (SaveEvent, bool, error)) (Outcome, error) {
	if p == nil || pageID == "" {
		return Skipped, nil
	}
	p.mu.Lock()
	if p.pages[pageID] != nil {
		p.mu.Unlock()
		return Superseded, nil
	}
	st := &pageState{gen: 1}
	p.pages[pageID] = st
	p.mu.Unlock()

	st.run.Lock()
	defer func() {
		st.run.Unlock()
		p.mu.Lock()
		if p.pages[pageID] == st && st.gen == 1 && st.timer == nil {
			delete(p.pages, pageID)
		}
		p.mu.Unlock()
	}()

	evt, ok, err := load(ctx)
	if err != nil {
		return Failed, err
	}
	if !ok || skipBook(evt.BookSlug) {
		return Skipped, nil
	}
	return p.ingest(ctx, evt, st, 1), nil
}

// OnPageDeleted clears memory rows for the page, and stops any pending
// or in-flight ingestion of it from committing afterwards. We leave
// the relationship triples behind — they may have been authored by
// other pages that mention this page's entities, and there's no
// per-page provenance on the relationships table to know which rows
// to drop. Phase 2's smarter cleanup can address.
func (p *Pipeline) OnPageDeleted(ctx context.Context, evt DeleteEvent) {
	if p == nil {
		return
	}
	p.mu.Lock()
	st := p.pages[evt.PageID]
	if st != nil {
		st.gen++
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		delete(p.pages, evt.PageID)
	}
	p.mu.Unlock()
	if st != nil {
		// Wait out a run that is mid-commit, so its rows are there to
		// delete; any later check sees the bumped gen and discards.
		st.commit.Lock()
		defer st.commit.Unlock()
	}
	if p.deps.MemoryStore == nil || evt.PageID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	scopeTag := scopeTagFor(evt.BookID)
	sourceRef := pageSourceRef(evt.PageID)
	if n, err := p.deps.MemoryStore.DeleteMemoriesBySource(ctx, "wiki_page", sourceRef, scopeTag); err != nil {
		log.Printf("[wikiknowledge] delete cleanup failed for %s: %v", sourceRef, err)
	} else if n > 0 {
		log.Printf("[wikiknowledge] cleared %d facts for deleted page %s", n, sourceRef)
	}
}

// ── Internals ─────────────────────────────────────────────────────

// pageSourceRef is the source_ref a page's facts carry. It is the page
// id, not its slug: slugs change on every title edit, and facts keyed
// by slug were orphaned by a rename.
func pageSourceRef(pageID string) string {
	return "page:" + pageID
}

func (p *Pipeline) buildFacts(ctx context.Context, evt SaveEvent, scopeTag, sourceRef string, facts []sidecar.ExtractedFact) []*pb.FactProto {
	now := time.Now()
	pbFacts := make([]*pb.FactProto, 0, len(facts))
	for _, f := range facts {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		var emb []float32
		if p.deps.Embedder != nil {
			if v, err := p.deps.Embedder(ctx, content); err == nil {
				emb = v
			}
		}
		tags := []string{scopeTag}
		if f.Category != "" {
			tags = append(tags, f.Category)
		}
		// UserId is the saver, who owns the row (console, dedup);
		// recall of a "book:" row follows the book's current
		// membership, not this id (memory.recallVisible).
		pbFacts = append(pbFacts, &pb.FactProto{
			Id:                uuid.NewString(),
			Content:           content,
			Embedding:         emb,
			SourceType:        "wiki_page",
			SourceRef:         sourceRef,
			SourceDescription: "Extracted from wiki page " + evt.Title,
			Confidence:        0.9,
			ConfidenceBasis:   "directly_observed",
			Scope:             "user",
			UserId:            evt.UserID,
			Tags:              tags,
			CreatedAt:         timestamppb.New(now),
			LastAccessed:      timestamppb.New(now),
			ScopeTag:          scopeTag,
		})
	}
	return pbFacts
}

func (p *Pipeline) upsertExtractedRelationships(ctx context.Context, evt SaveEvent, scopeTag string, rels []sidecar.ExtractedRelationship) {
	if p.deps.RelStore == nil || len(rels) == 0 {
		return
	}
	out := make([]memory.Relationship, 0, len(rels))
	for _, r := range rels {
		if r.Subject == "" || r.Predicate == "" || r.Object == "" {
			continue
		}
		out = append(out, memory.Relationship{
			Subject:    r.Subject,
			Predicate:  r.Predicate,
			Object:     r.Object,
			UserID:     evt.UserID,
			ScopeTag:   scopeTag,
			Confidence: 0.9,
		})
	}
	if len(out) == 0 {
		return
	}
	if err := p.deps.RelStore.UpsertRelationships(ctx, out); err != nil {
		log.Printf("[wikiknowledge] upsert relationships failed for %s/%s: %v",
			evt.BookSlug, evt.PageSlug, err)
	}
}

// upsertLinkTriples sets the page's "links_to" triples to its resolved
// outbound links, one per link. Subjects + objects use the canonical
// "page:{book_slug}/{page_slug}" form so they sit alongside
// content-extracted entities in the same relationship graph. The set
// is replaced, not added to: a link removed from the page loses its
// edge, and a page saved with no links has none (it used to keep
// whatever it had).
func (p *Pipeline) upsertLinkTriples(ctx context.Context, evt SaveEvent, scopeTag string) {
	if p.deps.RelStore == nil {
		return
	}
	var objects []string
	for _, l := range evt.Links {
		if l.TargetPageID == nil {
			continue // broken link
		}
		targetBook := l.TargetBookSlug
		if targetBook == "" {
			targetBook = evt.BookSlug
		}
		objects = append(objects, pageEntityKey(targetBook, l.TargetPageSlug))
	}
	subject := pageEntityKey(evt.BookSlug, evt.PageSlug)
	if err := p.deps.RelStore.ReplacePageLinks(ctx, subject, evt.UserID, scopeTag, objects); err != nil {
		log.Printf("[wikiknowledge] replace link triples failed for %s/%s: %v",
			evt.BookSlug, evt.PageSlug, err)
	}
}

func scopeTagFor(bookID string) string {
	return "book:" + bookID
}

func pageEntityKey(bookSlug, pageSlug string) string {
	return fmt.Sprintf("page:%s/%s", bookSlug, pageSlug)
}
