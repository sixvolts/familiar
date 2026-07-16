// Package wikiknowledge runs the async knowledge-ingestion pipeline
// for wiki pages (BOOKS-WIKI-ARCHITECTURE Phase 1 step 6).
//
// On every page save:
//
//  1. DELETE any prior memory rows for this page (source_ref +
//     scope_tag) so re-ingest is a clean replace, not an
//     accumulation.
//  2. Hand the page body to the sidecar's ExtractFacts (same
//     extraction it uses on conversation turns) and CommitFacts
//     each result with scope_tag = "book:{id}", source_type =
//     "wiki_page", source_ref = "{book_slug}/{page_slug}".
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
	"time"

	"github.com/familiar/gateway/internal/admin"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"

	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EngineClient is the slice of engine.Service we depend on. Defined
// here so tests can stub without dragging the real memengine in.
type EngineClient interface {
	CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error)
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
// for stale-fact cleanup.
type MemoryStore interface {
	DeleteMemoriesBySource(ctx context.Context, sourceType, sourceRef, scopeTag string) (int64, error)
}

// RelationshipStore is the slice of *memory.PgRelationshipStore we
// depend on for triple persistence.
type RelationshipStore interface {
	UpsertRelationships(ctx context.Context, rels []memory.Relationship) error
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

	// Timeout caps the entire OnPageSaved run. Set conservatively
	// because the goroutine fires from request context.Background()
	// and we don't want runaway sidecar calls to leak forever. Zero
	// uses a sensible default.
	Timeout time.Duration
}

// Pipeline is the wiki knowledge runner. Construct one per gateway
// process and pass its OnPageSaved / OnPageDeleted methods to
// admin.WikiStore.SetPageSaveHook / SetPageDeleteHook.
type Pipeline struct {
	deps Deps
}

// New constructs a Pipeline. Returns nil if Deps is empty enough
// that no work could happen.
func New(deps Deps) *Pipeline {
	if deps.Timeout <= 0 {
		deps.Timeout = 30 * time.Second
	}
	if deps.Engine == nil && deps.Sidecar == nil && deps.MemoryStore == nil && deps.RelStore == nil {
		return nil
	}
	return &Pipeline{deps: deps}
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

// OnPageSaved runs the full ingestion sequence for one page.
// Synchronous from the caller's POV — but since the wiki store's
// hook invokes us with context.Background() in a goroutine, this
// is effectively async with respect to the original HTTP request.
//
// Step ordering matters:
//  1. Stale cleanup BEFORE re-ingest, otherwise old facts persist
//     alongside the fresh ones until the next save.
//  2. Embed + commit facts BEFORE relationships, so a relationship
//     that references a fresh fact id (Phase 2 work) finds it.
//  3. Wiki link triples LAST, after the extracted relationships,
//     so a links_to edge to a page that's also mentioned in the
//     content overwrites cleanly.
func (p *Pipeline) OnPageSaved(ctx context.Context, evt SaveEvent) {
	if p == nil {
		return
	}
	// Research evidence books (slug research:{userID}) hold transient
	// raw web scratch that gets reaped, not knowledge worth remembering
	// — extracting facts from it would pollute memory with low-value,
	// possibly prompt-injected content (RESEARCH-SKILL-SPEC §9) and
	// leave facts orphaned when the sweep deletes the page. Skip them
	// entirely: no extraction, so nothing to clean up either.
	if strings.HasPrefix(evt.BookSlug, "research:") {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.deps.Timeout)
	defer cancel()

	scopeTag := scopeTagFor(evt.BookID)
	sourceRef := evt.BookSlug + "/" + evt.PageSlug

	// 1. Stale cleanup.
	if p.deps.MemoryStore != nil {
		if n, err := p.deps.MemoryStore.DeleteMemoriesBySource(ctx, "wiki_page", sourceRef, scopeTag); err != nil {
			log.Printf("[wikiknowledge] stale cleanup failed for %s: %v", sourceRef, err)
		} else if n > 0 {
			log.Printf("[wikiknowledge] cleared %d stale facts for %s", n, sourceRef)
		}
	}

	// 2. Extract → commit facts. ExtractFacts takes []Turn — we
	// wrap the page as a single user turn with the title for
	// context. Skip the call if the body is too small to be worth
	// the LLM round-trip (an empty page or a one-word note isn't
	// going to yield meaningful triples).
	body := strings.TrimSpace(evt.Content)
	if p.deps.Sidecar != nil && len(body) > 32 {
		turns := []sidecar.Turn{
			{Role: "user", Content: "Page title: " + evt.Title + "\n\n" + body},
		}
		// Large documents (research write-ups) overrun the small extract
		// model — route them to the big-model extract, which falls back
		// to the small one when no large route is configured.
		var extraction sidecar.ExtractionResult
		var err error
		if len(body) > largeExtractChars {
			extraction, err = p.deps.Sidecar.ExtractFactsLarge(ctx, turns)
		} else {
			extraction, err = p.deps.Sidecar.ExtractFacts(ctx, turns)
		}
		if err != nil {
			log.Printf("[wikiknowledge] extract failed for %s: %v", sourceRef, err)
		} else {
			p.commitExtractedFacts(ctx, evt, scopeTag, sourceRef, extraction.Facts)
			p.upsertExtractedRelationships(ctx, evt, scopeTag, extraction.Relationships)
		}
	}

	// 3. Wiki link triples for resolved outbound links.
	p.upsertLinkTriples(ctx, evt, scopeTag)
}

// OnPageDeleted clears memory rows for the page. We leave the
// relationship triples behind — they may have been authored by
// other pages that mention this page's entities, and there's no
// per-page provenance on the relationships table to know which
// rows to drop. Phase 2's smarter cleanup can address.
func (p *Pipeline) OnPageDeleted(ctx context.Context, evt DeleteEvent) {
	if p == nil || p.deps.MemoryStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.deps.Timeout)
	defer cancel()

	scopeTag := scopeTagFor(evt.BookID)
	sourceRef := evt.BookSlug + "/" + evt.PageSlug
	if n, err := p.deps.MemoryStore.DeleteMemoriesBySource(ctx, "wiki_page", sourceRef, scopeTag); err != nil {
		log.Printf("[wikiknowledge] delete cleanup failed for %s: %v", sourceRef, err)
	} else if n > 0 {
		log.Printf("[wikiknowledge] cleared %d facts for deleted page %s", n, sourceRef)
	}
}

// ── Internals ─────────────────────────────────────────────────────

func (p *Pipeline) commitExtractedFacts(ctx context.Context, evt SaveEvent, scopeTag, sourceRef string, facts []sidecar.ExtractedFact) {
	if p.deps.Engine == nil || len(facts) == 0 {
		return
	}
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
	if len(pbFacts) == 0 {
		return
	}
	// SessionID is a routing bucket on the engine side; book scope
	// is the actual visibility key (carried in ScopeTag). Use a
	// synthetic per-book id so wiki commits from different books
	// stay logically separated in the engine.
	sessionID := "wiki:" + evt.BookID
	if _, err := p.deps.Engine.CommitFacts(ctx, sessionID, pbFacts); err != nil {
		log.Printf("[wikiknowledge] CommitFacts failed for %s: %v", sourceRef, err)
		return
	}
	log.Printf("[wikiknowledge] committed %d facts for %s", len(pbFacts), sourceRef)
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

// upsertLinkTriples emits one "links_to" triple per resolved
// outbound link. Subjects + objects use the canonical
// "page:{book_slug}/{page_slug}" form so they sit alongside
// content-extracted entities in the same relationship graph.
func (p *Pipeline) upsertLinkTriples(ctx context.Context, evt SaveEvent, scopeTag string) {
	if p.deps.RelStore == nil || len(evt.Links) == 0 {
		return
	}
	out := make([]memory.Relationship, 0, len(evt.Links))
	subject := pageEntityKey(evt.BookSlug, evt.PageSlug)
	for _, l := range evt.Links {
		if l.TargetPageID == nil {
			continue // broken link
		}
		targetBook := l.TargetBookSlug
		if targetBook == "" {
			targetBook = evt.BookSlug
		}
		out = append(out, memory.Relationship{
			Subject:    subject,
			Predicate:  "links_to",
			Object:     pageEntityKey(targetBook, l.TargetPageSlug),
			UserID:     evt.UserID,
			ScopeTag:   scopeTag,
			Confidence: 1.0,
		})
	}
	if len(out) == 0 {
		return
	}
	if err := p.deps.RelStore.UpsertRelationships(ctx, out); err != nil {
		log.Printf("[wikiknowledge] upsert link triples failed for %s/%s: %v",
			evt.BookSlug, evt.PageSlug, err)
	}
}

func scopeTagFor(bookID string) string {
	return "book:" + bookID
}

func pageEntityKey(bookSlug, pageSlug string) string {
	return fmt.Sprintf("page:%s/%s", bookSlug, pageSlug)
}
