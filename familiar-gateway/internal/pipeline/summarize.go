package pipeline

import (
	"context"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/sidecar"
	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// recoverBackground contains a panic in a detached pipeline goroutine.
// runSummarize / runPostTurnExtract parse model-shaped output (the
// least trustworthy input in the system) on their own goroutines, and
// a panic there — unlike one inside an http handler, which net/http
// absorbs — takes the entire gateway down. Log it (with the stack) and
// let the goroutine die; the next turn re-summarizes/re-extracts fresh.
func recoverBackground(label string) {
	if r := recover(); r != nil {
		log.Printf("[pipeline] %s panic recovered: %v\n%s", label, r, debug.Stack())
	}
}

const (
	// VerbatimWindow is how many turns (user+assistant each count as one) are
	// kept verbatim in the session. When Turns exceeds this, older turns get
	// folded into the rolling summary.
	//
	// Sized for modern context windows: 24 turns (~12 user/assistant
	// exchanges) at average ~250 tokens each ≈ 6K tokens. Easily fits
	// inside the 32K knowledge tier conv budget and well below the
	// 64K deep_reasoning budget. Keeps recent detail intact instead
	// of compacting after only 3 exchanges. Bump higher if your
	// typical conversations run longer; a tier-aware variant is
	// possible but Phase 1 of the model-roles refactor will
	// supersede this constant entirely.
	VerbatimWindow = 24

	// SummarizeBatch is how many turns get summarized away per pass, once the
	// threshold fires. Leaving a few turns above the verbatim window buffers
	// against the next trigger.
	SummarizeBatch = 8

	// noopDedupThreshold is the cosine similarity above which an extracted
	// fact is treated as an outright duplicate and skipped without asking
	// the medium slot. Above this we trust the embedding alone — paying
	// for a batch-classify call to confirm a near-perfect-match neighbor
	// is wasted latency.
	noopDedupThreshold = 0.92
)

// maybeSummarize fires an async summarization pass if the session has
// accumulated more turns than the verbatim window permits. Safe to call
// after every response delivery — it no-ops when not needed and guards
// against concurrent summarization for the same session.
//
// `overrides` is non-nil only for shard invocations; the goroutine
// captures it so the rolling-summary save and the extracted FactProtos
// downstream both carry the shard's scope_tag.
func (p *Pipeline) maybeSummarize(sess *session.Session, overrides *ShardOverrides) {
	if p.sidecarClient == nil {
		return
	}
	_, turnCount := sess.Snapshot()
	if turnCount <= VerbatimWindow {
		return
	}
	if !sess.TryBeginSummarize() {
		return // another goroutine is already summarizing this session
	}
	go p.runSummarize(sess, overrides)
}

// runSummarize does the async summarization + fact extraction for a session.
// Runs in its own goroutine, uses a fresh context so it's not tied to the
// request that triggered it.
func (p *Pipeline) runSummarize(sess *session.Session, overrides *ShardOverrides) {
	defer recoverBackground("runSummarize")
	defer sess.EndSummarize()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()

	// Pick the N oldest turns to fold into the summary. The rest stay verbatim.
	_, total := sess.Snapshot()
	dropCount := total - VerbatimWindow
	if dropCount <= 0 {
		return
	}
	if dropCount > SummarizeBatch {
		dropCount = SummarizeBatch
	}

	// Snapshot the turns we intend to summarize without mutating yet — if the
	// sidecar call fails, leave the session untouched.
	prevSummary, localTurns := sess.SnapshotForSummarize(dropCount)
	toSummarize := make([]sidecar.Turn, len(localTurns))
	for i, t := range localTurns {
		toSummarize[i] = sidecar.Turn{Role: t.Role, Content: t.Content}
	}
	dropCount = len(localTurns)

	p.events.Emit(sess.ID, memevents.KindCompactionStarted, memevents.CompactionStartedPayload{
		TurnCount:     dropCount,
		OldestTurnAge: total,
	})

	newSummary, err := p.sidecarClient.Summarize(ctx, prevSummary, toSummarize)
	if err != nil {
		log.Printf("[pipeline] summarize failed for session %s: %v", sess.ID, err)
		p.events.Emit(sess.ID, memevents.KindCompactionFailed, memevents.CompactionFailedPayload{
			Stage:    "summarize",
			Reason:   err.Error(),
			Deferred: true,
		})
		return
	}

	dropped := sess.CompactSummary(newSummary, dropCount)
	log.Printf("[pipeline] summarized %d turns for session %s (summary: %d chars)",
		len(dropped), sess.ID, len(newSummary))
	p.events.Emit(sess.ID, memevents.KindSummaryGenerated, memevents.SummaryGeneratedPayload{
		TokensIn:       approxTokenCount(toSummarize),
		TokensOut:      len(newSummary) / 4, // rough proxy; precise counts need a tokenizer
		SummaryPreview: previewString(newSummary, 200),
	})

	// Persist the rolling summary so a gateway restart doesn't lose it.
	if p.sessionStore != nil {
		saveCtx, saveCancel := context.WithTimeout(ctx, 3*time.Second)
		if err := p.sessionStore.Save(saveCtx,
			sess.SummaryKey(),
			newSummary,
			sess.SummarizedCountSnapshot(),
			scopeTagFor(overrides),
		); err != nil {
			log.Printf("[pipeline] session store save %s: %v", sess.ID, err)
		}
		saveCancel()
	}

	// CHAT-REARCH §"Memory Write Pipeline" decouples extraction from
	// compaction: facts are extracted per-turn by commitAndExtract, so
	// runSummarize stops here. The dropped turns have already been
	// extracted in earlier per-turn passes.
	_ = dropped

	p.events.Emit(sess.ID, memevents.KindCompactionCompleted, memevents.CompactionCompletedPayload{
		DurationMs: int(time.Since(start) / time.Millisecond),
	})
}

// approxTokenCount is a rough char/4 proxy used only for the
// SummaryGenerated event payload. Precise token counts need a
// tokenizer matched to the chat model — not worth the dep weight
// for an observability counter.
func approxTokenCount(turns []sidecar.Turn) int {
	total := 0
	for _, t := range turns {
		total += len(t.Content)
	}
	return total / 4
}

func previewString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// postTurnDeadline is the soft budget for the entire post-turn write
// pipeline (fact extraction → batched conflict + relationship pass →
// commit). On miss the goroutine logs and drops; the next turn runs
// fresh. Per CHAT-REARCH §"Soft Deadline" — we'd rather store an
// occasional duplicate than block the write pipeline indefinitely.
const postTurnDeadline = 10 * time.Second

// kickoffPostTurnExtract fires the post-turn write pipeline in a
// fresh goroutine with a 10s soft deadline. Returns immediately so
// the user-facing response isn't held up. Safe to call when the
// sidecar is unavailable (no-ops).
func (p *Pipeline) kickoffPostTurnExtract(sess *session.Session, userMsg, responseText string, retrievedRels []memory.Relationship, overrides *ShardOverrides) {
	if p.sidecarClient == nil {
		return
	}
	if strings.TrimSpace(userMsg) == "" && strings.TrimSpace(responseText) == "" {
		return
	}
	go p.runPostTurnExtract(sess, userMsg, responseText, retrievedRels, overrides)
}

// runPostTurnExtract executes the per-turn memory write pipeline:
//  1. Fact extraction on the small slot, fed the (user, assistant)
//     pair (CHAT-REARCH spec input).
//  2. Per-candidate nearest-neighbor lookup via memStore.
//  3. Single batched conflict + relationship pass on the medium slot.
//  4. Apply decisions: ADD → commit; UPDATE → commit with Supersedes;
//     DUPLICATE → skip. Upsert relationships emitted by the batch.
//
// Wraps the whole flow in a 10s deadline. Any miss logs and exits;
// the next turn re-extracts fresh. Best-effort — every step is
// allowed to fail without rolling back earlier writes.
func (p *Pipeline) runPostTurnExtract(sess *session.Session, userMsg, responseText string, retrievedRels []memory.Relationship, overrides *ShardOverrides) {
	defer recoverBackground("runPostTurnExtract")
	// No durable extract without a resolved identity. pgvector and the
	// relationships table both treat a NULL/empty user_id as "visible to
	// every user" (pgvector.go: `user_id IS NULL OR user_id = $n`), so a
	// fact or edge written with an empty owner leaks across tenants. The
	// conversation-fact path (commitAndExtract) already guards this; mirror
	// it here for the extracted facts AND the relationship edges, which
	// have no DB CHECK of their own on older deploys. Trusted surfaces
	// always carry a resolved id; this only fires for unresolved-identity
	// adapters (e.g. CLI). See EXTERNAL-READINESS-REVIEW.md P0.
	if sess.UserID() == "" {
		log.Printf("[pipeline] skip post-turn extract for session %s: no resolved identity (would be globally visible)", sess.ID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), postTurnDeadline)
	defer cancel()
	start := time.Now()

	// Step 1: extract candidate facts + initial relationships from
	// this turn pair. Routes to the small (or small_async) slot.
	turns := []sidecar.Turn{
		{Role: "user", Content: userMsg},
		{Role: "assistant", Content: responseText},
	}
	extraction, err := p.sidecarClient.ExtractFacts(ctx, turns)
	if err != nil {
		deferred := ctx.Err() != nil
		if deferred {
			log.Printf("[pipeline] post-turn extract deadline (extract) for session %s: %v", sess.ID, ctx.Err())
		} else {
			log.Printf("[pipeline] post-turn extract failed for session %s: %v", sess.ID, err)
		}
		p.events.Emit(sess.ID, memevents.KindCompactionFailed, memevents.CompactionFailedPayload{
			Stage:    "extract",
			Reason:   err.Error(),
			Deferred: deferred,
		})
		return
	}
	candidates := extraction.Facts
	if len(candidates) == 0 && len(extraction.Relationships) == 0 {
		return
	}

	// Step 2: build BatchExtractInput. Embed each candidate up front so
	// the same vector serves the neighbor lookup AND the eventual
	// FactProto commit, avoiding a second embed call. Empty-content
	// candidates get skipped.
	type prepared struct {
		fact      sidecar.ExtractedFact
		embedding []float32
		neighbor  memory.NearestFact
		hasNbr    bool
	}
	prep := make([]prepared, 0, len(candidates))
	batchCands := make([]sidecar.BatchCandidate, 0, len(candidates))

	for _, f := range candidates {
		if f.Content == "" {
			continue
		}
		emb := p.embedText(ctx, f.Content)
		bc := sidecar.BatchCandidate{Fact: f}

		var nf memory.NearestFact
		var ok bool
		if p.memStore != nil && len(emb) > 0 {
			lookupCtx, lookupCancel := context.WithTimeout(ctx, 2*time.Second)
			n, found, lerr := p.memStore.NearestLiveFact(lookupCtx, emb, sess.UserID())
			lookupCancel()
			if lerr == nil && found {
				nf = n
				ok = true
				bc.Neighbors = []sidecar.FactNeighbor{
					{ID: n.ID, Content: n.Content, Similarity: n.Similarity},
				}
			}
		}

		// Cheap dedupe before paying for the medium-slot call: if a
		// candidate is essentially identical to its nearest neighbor,
		// skip it without asking the model.
		if ok && nf.Similarity >= noopDedupThreshold {
			continue
		}

		prep = append(prep, prepared{fact: f, embedding: emb, neighbor: nf, hasNbr: ok})
		batchCands = append(batchCands, bc)
	}

	// Step 3: batched conflict + relationship pass. Skip the call when
	// there are no candidates AND no rels to enrich — there's nothing
	// for the medium slot to do.
	var batch sidecar.BatchExtractResult
	if len(batchCands) > 0 {
		retrievedTriples := make([]sidecar.ExtractedRelationship, 0, len(retrievedRels))
		for _, r := range retrievedRels {
			retrievedTriples = append(retrievedTriples, sidecar.ExtractedRelationship{
				Subject:   r.Subject,
				Predicate: r.Predicate,
				Object:    r.Object,
			})
		}
		batchIn := sidecar.BatchExtractInput{
			UserMessage:      userMsg,
			AssistantMessage: responseText,
			Candidates:       batchCands,
			RetrievedRels:    retrievedTriples,
		}
		batchResult, berr := p.sidecarClient.BatchClassifyAndRelate(ctx, batchIn)
		if berr != nil {
			if ctx.Err() != nil {
				log.Printf("[pipeline] post-turn extract deadline (batch) for session %s: %v", sess.ID, ctx.Err())
				return
			}
			log.Printf("[pipeline] batch classify failed for session %s: %v (defaulting all to ADD)", sess.ID, berr)
			// Synthesize ADD decisions so we still commit the
			// candidates — preferring duplicates to dropped writes.
			batchResult.Decisions = make([]sidecar.BatchDecision, len(batchCands))
			for i := range batchResult.Decisions {
				batchResult.Decisions[i].Action = "ADD"
			}
		}
		batch = batchResult
	}

	// Step 4: apply decisions. Decisions[i] applies to prep[i]; if the
	// model returned fewer than len(prep), missing slots default to ADD.
	now := time.Now()
	convTag := "conv:" + sess.ID
	pbFacts := make([]*pb.FactProto, 0, len(prep))
	var skipped, updated int

	for i, p2 := range prep {
		action, targetID := resolveDecision(i, batch.Decisions, p2.hasNbr, p2.neighbor.ID)
		switch action {
		case "DUPLICATE":
			skipped++
			p.events.Emit(sess.ID, memevents.KindConflictResolved, memevents.ConflictResolvedPayload{
				Action:      "DUPLICATE",
				TargetID:    targetID,
				FactPreview: previewString(p2.fact.Content, 200),
			})
			continue
		case "UPDATE":
			updated++
		}

		fact := &pb.FactProto{
			Id:                uuid.NewString(),
			Content:           p2.fact.Content,
			Embedding:         p2.embedding,
			SourceType:        "conversation_extraction",
			SourceRef:         sess.ID,
			SourceDescription: "Extracted from conversation",
			Confidence:        0.9,
			ConfidenceBasis:   "directly_observed",
			Scope:             "user",
			UserId:            sess.UserID(),
			Tags:              []string{convTag, p2.fact.Category},
			Supersedes:        supersedesFor(action, targetID),
			CreatedAt:         timestamppb.New(now),
			LastAccessed:      timestamppb.New(now),
			ScopeTag:          scopeTagFor(overrides),
			ExcludeFromHot:    excludeFromHotFor(overrides),
		}
		pbFacts = append(pbFacts, fact)
		p.events.Emit(sess.ID, memevents.KindConflictResolved, memevents.ConflictResolvedPayload{
			Action:      action,
			FactID:      fact.Id,
			TargetID:    targetID,
			FactPreview: previewString(p2.fact.Content, 200),
		})
		p.events.Emit(sess.ID, memevents.KindFactExtracted, memevents.FactExtractedPayload{
			FactID:   fact.Id,
			Content:  p2.fact.Content,
			Category: p2.fact.Category,
		})
	}
	if skipped > 0 || updated > 0 {
		log.Printf("[pipeline] post-turn extract for session %s: candidates=%d skipped=%d updated=%d",
			sess.ID, len(prep), skipped, updated)
	}

	if len(pbFacts) > 0 {
		commitCtx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := p.engine.CommitFacts(commitCtx, sess.ID, pbFacts); err != nil {
			ccancel()
			log.Printf("[pipeline] CommitFacts (post-turn) error for session %s: %v", sess.ID, err)
			p.events.Emit(sess.ID, memevents.KindCompactionFailed, memevents.CompactionFailedPayload{
				Stage:    "commit",
				Reason:   err.Error(),
				Deferred: ctx.Err() != nil,
			})
			return
		}
		ccancel()
		log.Printf("[pipeline] committed %d post-turn facts for session %s", len(pbFacts), sess.ID)

		if p.versioner != nil {
			recordVersions(ctx, p.versioner, pbFacts)
		}
	}

	// Relationships from the batched call OR the original extraction.
	// Prefer the batch's output (richer context); fall back to the
	// extractor's only when the batch ran with no candidates.
	rels := batch.Relationships
	if len(rels) == 0 {
		rels = extraction.Relationships
	}
	if len(rels) > 0 && p.relStore != nil {
		var provenance string
		if len(pbFacts) > 0 {
			provenance = pbFacts[0].Id
		}
		out := make([]memory.Relationship, 0, len(rels))
		for _, r := range rels {
			out = append(out, memory.Relationship{
				Subject:    r.Subject,
				Predicate:  r.Predicate,
				Object:     r.Object,
				UserID:     sess.UserID(),
				SourceFact: provenance,
				Confidence: 0.9,
				// Stamp the shard scope, exactly as the memories from
				// this same turn are (see the FactProto above). Without
				// it, an isolated shard's memories are hidden from
				// top-level retrieval but the triples extracted from the
				// same turns leaked into the top-level prompt via graph
				// augmentation.
				ScopeTag: scopeTagFor(overrides),
			})
		}
		relCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := p.relStore.UpsertRelationships(relCtx, out); err != nil {
			log.Printf("[pipeline] upsert relationships for session %s: %v", sess.ID, err)
		} else {
			log.Printf("[pipeline] upserted %d relationships for session %s", len(out), sess.ID)
			for _, r := range out {
				p.events.Emit(sess.ID, memevents.KindRelationshipAdded, memevents.RelationshipAddedPayload{
					From:      r.Subject,
					Predicate: r.Predicate,
					To:        r.Object,
				})
			}
		}
		cancel()
	}

	p.events.Emit(sess.ID, memevents.KindCompactionCompleted, memevents.CompactionCompletedPayload{
		DurationMs:        int(time.Since(start) / time.Millisecond),
		FactsAdded:        len(pbFacts) - updated,
		ConflictsResolved: skipped + updated,
	})
}

// resolveDecision picks the commit action and supersede target for the
// i-th extracted candidate given the medium slot's batch decisions.
//
// Two invariants worth locking down (they're the index/fallback logic
// the post-turn extract path is most likely to get wrong):
//   - The model can return fewer decisions than candidates. A
//     past-the-end index defaults to ADD — we'd rather commit a
//     possible duplicate than silently drop an extracted fact.
//   - An UPDATE with no explicit TargetID falls back to the
//     candidate's nearest neighbor (when one was found), since
//     "update" is meaningless without a target to supersede.
//
// The returned targetID is the model's raw target (used by the
// DUPLICATE emit for observability). The commit path only promotes it
// to Supersedes when action == "UPDATE" — see the apply loop; an ADD
// or unknown action must not supersede anything even if the model
// supplied a target_id.
// supersedesFor promotes a resolved target to a Supersedes pointer
// only for a genuine UPDATE. Everywhere in the system a pointed-at row
// is HIDDEN from retrieval, so an ADD — or any unrecognized action the
// local model emitted (e.g. "CONTRADICTS") that carried a target_id —
// must not supersede anything, or it would silently bury an unrelated
// live fact. Only UPDATE means "replace this specific row."
func supersedesFor(action, targetID string) string {
	if action == "UPDATE" {
		return targetID
	}
	return ""
}

func resolveDecision(i int, decisions []sidecar.BatchDecision, hasNeighbor bool, neighborID string) (action, targetID string) {
	action = "ADD"
	if i < len(decisions) {
		action = decisions[i].Action
		targetID = decisions[i].TargetID
	}
	if action == "UPDATE" && targetID == "" && hasNeighbor {
		targetID = neighborID
	}
	return action, targetID
}

// recordVersions writes admin-timeline rows for a batch of committed
// facts. Each ADD records "created"; each UPDATE (fact.Supersedes set)
// records "superseded" on the old memory and "created" on the new.
// Errors are logged but never block the write path.
func recordVersions(ctx context.Context, v MemoryVersioner, facts []*pb.FactProto) {
	for _, fact := range facts {
		if fact.Supersedes == "" {
			verCtx, vCancel := context.WithTimeout(ctx, 2*time.Second)
			if err := v.RecordVersion(verCtx, fact.Id, fact.Content,
				fact.Scope, fact.SourceType, "system:extraction", "created"); err != nil {
				log.Printf("[pipeline] record version (created) %s: %v", fact.Id, err)
			}
			vCancel()
			continue
		}
		verCtx, vCancel := context.WithTimeout(ctx, 2*time.Second)
		if err := v.RecordVersion(verCtx, fact.Supersedes, fact.Content,
			fact.Scope, fact.SourceType, "system:extraction", "superseded"); err != nil {
			log.Printf("[pipeline] record version (superseded) %s: %v", fact.Supersedes, err)
		}
		vCancel()
		verCtx2, vCancel2 := context.WithTimeout(ctx, 2*time.Second)
		if err := v.RecordVersion(verCtx2, fact.Id, fact.Content,
			fact.Scope, fact.SourceType, "system:extraction", "created"); err != nil {
			log.Printf("[pipeline] record version (created-new) %s: %v", fact.Id, err)
		}
		vCancel2()
	}
}
