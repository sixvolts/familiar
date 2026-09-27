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
//
// This calls recover() itself rather than delegating to safego.Recover:
// recover() only works when invoked DIRECTLY by the deferred function, so
// `defer recoverBackground(...)` -> safego.Recover() would sit one frame too
// deep and the panic would escape. Kept as a named wrapper because the two
// call sites read better with the label baked in.
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

	// SummarizeBatch is the most turns folded into the summary in one
	// pass; a pass folds everything above the verbatim window, up to this
	// many. It was a fixed 8, but one tool-heavy exchange adds 2 plus two
	// per tool call, so research turns outran it and the excess reached
	// the buffer's cap and fell off unsummarized.
	SummarizeBatch = 32

	// The identical-enough cutoff (above which an extracted fact is
	// skipped without asking the medium slot) and the supersede floor now
	// come from [memory].dedup_threshold / supersede_threshold via
	// MemoryConfig's *OrDefault accessors. dedup_threshold was previously
	// declared, validated, and read by nothing while this path hardcoded
	// 0.92 — the knob existed and did not work.
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
	// The persisted summary failed to load (hydration retries it): a
	// summary written now would start from nothing and replace it.
	if sess.SummaryUnknown() {
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
	if len(localTurns) == 0 {
		return
	}
	toSummarize := summarizerTurns(localTurns)
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

	dropped := sess.CompactSummary(newSummary, localTurns[len(localTurns)-1].Seq)
	log.Printf("[pipeline] summarized %d turns for session %s (summary: %d chars)",
		len(dropped), sess.ID, len(newSummary))
	p.events.Emit(sess.ID, memevents.KindSummaryGenerated, memevents.SummaryGeneratedPayload{
		TokensIn:       approxTokenCount(toSummarize),
		TokensOut:      len(newSummary) / 4, // rough proxy; precise counts need a tokenizer
		SummaryPreview: previewString(newSummary, 200),
	})

	// Persist the rolling summary so a gateway restart doesn't lose it,
	// unless the session was dropped meanwhile (its conversation was
	// deleted): saving would bring the deleted chat's summary back.
	if p.sessions != nil {
		if cur, ok := p.sessions.Get(sess.ID); !ok || cur != sess {
			log.Printf("[pipeline] session %s dropped during summarize; summary not saved", sess.ID)
			return
		}
	}
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

// summarizerTurns renders buffered turns for the summarizer. A tool
// result becomes a line naming the tool, and a tool-calling turn its
// prose and the list of calls. Results are whatever a web page or a
// shared page said, and the summary is replayed in the system message
// of every later turn (and persisted), where an instruction planted in
// a page reads as the user's own. The results themselves stay in the
// verbatim history the model sees as tool messages.
func summarizerTurns(turns []session.Turn) []sidecar.Turn {
	out := make([]sidecar.Turn, 0, len(turns))
	names := map[string]string{} // tool call id -> tool name
	for _, t := range turns {
		switch {
		case t.Role == "tool":
			name := names[t.ToolCallID]
			if name == "" {
				name = "a tool"
			}
			out = append(out, sidecar.Turn{Role: "tool", Content: "[" + name + " result omitted]"})
		case len(t.ToolCalls) > 0:
			var called []string
			for _, c := range unmarshalToolCalls(t.ToolCalls) {
				names[c.ID] = c.Name
				called = append(called, c.Name)
			}
			line := "[called " + strings.Join(called, ", ") + "]"
			if prose := strings.TrimSpace(t.Content); prose != "" {
				line = prose + "\n" + line
			}
			out = append(out, sidecar.Turn{Role: t.Role, Content: line})
		default:
			out = append(out, sidecar.Turn{Role: t.Role, Content: t.Content})
		}
	}
	return out
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

// postTurnDeadline is the backstop for the post-turn write pipeline
// (fact extraction → batched conflict + relationship pass → commit),
// including any wait for the sidecar's slot behind other background
// work. Each sidecar request is bounded by [sidecar].request_timeout_ms
// on its own. It was 10s for the whole pipeline, gate wait included, and
// a miss during the batch dropped every candidate: the facts the user
// had just stated were never stored. A variable so tests can shorten it.
var postTurnDeadline = 60 * time.Second

// postTurnCommitBudget bounds committing candidates after the backstop
// fired.
const postTurnCommitBudget = 10 * time.Second

// extractContextTurns is how many prior turns we hand the extractor as
// read-only context so it can resolve pronouns/back-references in the current
// turn ("bump it to 64GB") into standalone facts. The extractor is told to
// resolve against this block but extract only from the current turn pair.
const extractContextTurns = 6

// kickoffPostTurnExtract fires the post-turn write pipeline in a
// fresh goroutine with a 10s soft deadline. Returns immediately so
// the user-facing response isn't held up. Safe to call when the
// sidecar is unavailable (no-ops).
func (p *Pipeline) kickoffPostTurnExtract(sess *session.Session, userMsg, responseText string, prior []sidecar.Turn, retrievedRels []memory.Relationship, overrides *ShardOverrides) {
	if p.sidecarClient == nil {
		return
	}
	if strings.TrimSpace(userMsg) == "" && strings.TrimSpace(responseText) == "" {
		return
	}
	go p.runPostTurnExtract(sess, userMsg, responseText, prior, retrievedRels, overrides)
}

// runPostTurnExtract executes the per-turn memory write pipeline:
//  1. Fact extraction on the small slot, fed the (user, assistant)
//     pair (CHAT-REARCH spec input).
//  2. Per-candidate nearest-neighbor lookup via memStore.
//  3. Single batched conflict + relationship pass on the medium slot.
//  4. Apply decisions: ADD → commit; UPDATE → commit with Supersedes;
//     DUPLICATE → skip. Upsert relationships emitted by the batch.
//
// Wraps the whole flow in postTurnDeadline. A miss before candidates
// exist logs and exits; a miss during the batched pass commits them as
// ADD, as a failed pass does (a duplicate is preferred to a lost fact).
// Best-effort — every step is allowed to fail without rolling back
// earlier writes.
//
// prior is the conversation before this turn (conversationTurns), which
// the extractor may resolve references against but not extract from.
func (p *Pipeline) runPostTurnExtract(sess *session.Session, userMsg, responseText string, prior []sidecar.Turn, retrievedRels []memory.Relationship, overrides *ShardOverrides) {
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
	// Prior turns, read-only, for reference resolution only. They used to be
	// the session's last 8 messages minus this turn's pair, which after a
	// tool-heavy turn were this turn's own tool results and tool-call stubs.
	extraction, err := p.sidecarClient.ExtractFactsWithContext(ctx, turns, prior)
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
	// conflictNeighborK is how many nearest live facts we show the conflict
	// classifier per candidate. Top-1 alone missed a supersede/dup target
	// sitting at rank 2-3 behind an unrelated closer neighbour.
	const conflictNeighborK = 5
	type prepared struct {
		fact      sidecar.ExtractedFact
		embedding []float32
		neighbors []memory.NearestFact // nearest live facts, most-similar first
	}
	prep := make([]prepared, 0, len(candidates))
	batchCands := make([]sidecar.BatchCandidate, 0, len(candidates))
	// Fact ids whose survivor we should reinforce because a candidate this turn
	// duplicated them — collected from both the cheap-gate skip below and the
	// model's DUPLICATE decisions, applied in one batch after the decision loop.
	var reinforceIDs []string

	for _, f := range candidates {
		if f.Content == "" {
			continue
		}
		emb := p.embedText(ctx, f.Content)
		bc := sidecar.BatchCandidate{Fact: f}

		var neighbors []memory.NearestFact
		if p.memStore != nil && len(emb) > 0 {
			lookupCtx, lookupCancel := context.WithTimeout(ctx, 2*time.Second)
			found, lerr := p.memStore.NearestLiveFacts(lookupCtx, emb, sess.UserID(), scopeTagFor(overrides), conflictNeighborK)
			lookupCancel()
			if lerr == nil && len(found) > 0 {
				neighbors = found
				bc.Neighbors = make([]sidecar.FactNeighbor, len(found))
				for j, n := range found {
					bc.Neighbors[j] = sidecar.FactNeighbor{ID: n.ID, Content: n.Content, Similarity: n.Similarity}
				}
			}
		}

		// Cheap dedupe before paying for the medium-slot call: if a candidate
		// is essentially identical to its NEAREST neighbor, skip it without
		// asking the model — but reinforce the survivor first (the user just
		// restated it, so bump its recency/frequency).
		if len(neighbors) > 0 && neighbors[0].Similarity >= p.memoryCfg.DedupThresholdOrDefault() {
			reinforceIDs = append(reinforceIDs, neighbors[0].ID)
			continue
		}

		prep = append(prep, prepared{fact: f, embedding: emb, neighbors: neighbors})
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
			// Recently-extracted facts as dedup/conflict context — catches a
			// restatement that hasn't yet landed in (or risen above the cosine
			// floor of) pgvector this session.
			RecentFacts: sess.RecentFacts(),
		}
		batchResult, berr := p.sidecarClient.BatchClassifyAndRelate(ctx, batchIn)
		if berr != nil {
			if ctx.Err() != nil {
				log.Printf("[pipeline] post-turn extract deadline (batch) for session %s: %v (committing candidates as ADD)", sess.ID, ctx.Err())
				// The rest runs on a fresh, short context: the expired
				// one would fail every write below.
				var cancelCommit context.CancelFunc
				ctx, cancelCommit = context.WithTimeout(context.Background(), postTurnCommitBudget)
				defer cancelCommit()
			} else {
				log.Printf("[pipeline] batch classify failed for session %s: %v (defaulting all to ADD)", sess.ID, berr)
			}
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

	// decided[i] is how pbFacts[i] was resolved; its events go out once
	// the commit says which row the fact landed on.
	type decision struct{ action, targetID, category string }
	var decided []decision
	for i, p2 := range prep {
		action, targetID := resolveDecision(i, batch.Decisions, p2.neighbors,
			p.memoryCfg.SupersedeThresholdOrDefault())
		switch action {
		case "DUPLICATE":
			skipped++
			if targetID != "" {
				reinforceIDs = append(reinforceIDs, targetID)
			}
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
		}
		pbFacts = append(pbFacts, fact)
		decided = append(decided, decision{action: action, targetID: targetID, category: p2.fact.Category})
	}
	if skipped > 0 || updated > 0 {
		log.Printf("[pipeline] post-turn extract for session %s: candidates=%d skipped=%d updated=%d",
			sess.ID, len(prep), skipped, updated)
	}

	// Reinforce the survivors of every duplicate detected this turn: bump
	// their recency/frequency so a restated fact doesn't look stale next to a
	// freshly-added near-neighbor. Best-effort — a failed bump never blocks
	// the commit below.
	if p.memStore != nil && len(reinforceIDs) > 0 {
		rCtx, rCancel := context.WithTimeout(ctx, 2*time.Second)
		if err := p.memStore.ReinforceFacts(rCtx, reinforceIDs); err != nil {
			log.Printf("[pipeline] reinforce %d duplicate target(s) for session %s: %v", len(reinforceIDs), sess.ID, err)
		}
		rCancel()
	}

	if len(pbFacts) > 0 {
		// Seed the session's recent ring with what we just extracted so the
		// next turn's batch extractor can dedup a restatement not yet in
		// pgvector (or below its cosine floor).
		contents := make([]string, len(pbFacts))
		for i, f := range pbFacts {
			contents[i] = f.Content
		}
		sess.AddRecentFacts(contents)

		// CommitFacts rewrites each fact's Id to the row that holds it: a
		// restated fact lands on its existing row. Keep the generated ids
		// to tell those apart.
		generated := make([]string, len(pbFacts))
		for i, f := range pbFacts {
			generated[i] = f.Id
		}
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

		// Announce the facts only now, with the ids they were stored
		// under; they used to go out before the commit, naming ids that
		// a restatement or a failed commit never created.
		for i, fact := range pbFacts {
			d := decided[i]
			p.events.Emit(sess.ID, memevents.KindConflictResolved, memevents.ConflictResolvedPayload{
				Action:      d.action,
				FactID:      fact.Id,
				TargetID:    d.targetID,
				FactPreview: previewString(fact.Content, 200),
			})
			p.events.Emit(sess.ID, memevents.KindFactExtracted, memevents.FactExtractedPayload{
				FactID:   fact.Id,
				Content:  fact.Content,
				Category: d.category,
			})
		}

		if p.versioner != nil {
			recordVersions(ctx, p.versioner, pbFacts, generated)
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

// resolveDecision turns one batch-classifier verdict into the action and
// supersede target actually applied, refusing anything the model was not
// entitled to say.
//
// The classifier is a small local model shown its candidate's up-to-K nearest
// live facts. So the only targets it can legitimately name are those shown
// neighbours' ids. It used to be taken at its
// word: decisions[i].TargetID was applied verbatim, with no check that it
// was that neighbour, a UUID, or even a row that exists, and an UPDATE with
// no target silently fell back to the neighbour whatever the similarity.
//
// Both halves were destructive, because every read path HIDES a pointed-at
// row. The neighbour lookup has no similarity threshold of its own, so any
// non-empty store always yields one: "I prefer dark mode" would be handed
// "the dog is named Rex" at cosine ~0.2, and one UPDATE with an empty
// target made the dog fact permanently invisible — recorded in the admin
// timeline as intentional.
//
// Two rules now:
//
//   - A NAMED target must be one of the shown neighbours. Anything else is a
//     hallucination, a stale id, or a target meant for a different
//     candidate in the batch; the write degrades to ADD rather than
//     superseding a row nobody looked at.
//   - An UNNAMED target on an UPDATE only falls back to the neighbour when
//     the neighbour clears supersedeFloor. Below that they are not the same
//     fact, so the honest outcome is two facts, not a hidden one.
//
// DUPLICATE gets the same target check, because dropping a write is also
// lossy: a DUPLICATE naming a hallucinated id used to discard the candidate
// entirely, which contradicts this path's own preference for a redundant
// fact over a lost one. It requires a neighbour to exist — a duplicate of
// nothing is not a duplicate. A DUPLICATE naming a shown neighbour needs no
// floor (the model compared the two); an UNNAMED one needs the nearest
// neighbour to clear supersedeFloor, like an unnamed UPDATE: "my dog is
// named Biscuit" was dropped as a duplicate of "prefers dark mode" at 0.21.
//
// The decision for candidate i is the one naming i as its candidate, or
// else the i-th (see decisionFor).
func resolveDecision(i int, decisions []sidecar.BatchDecision, neighbors []memory.NearestFact, supersedeFloor float64) (action, targetID string) {
	action = "ADD"
	if d, ok := decisionFor(i, decisions); ok {
		action = d.Action
		targetID = d.TargetID
	}
	hasNeighbor := len(neighbors) > 0

	// Rule 1: a named target must be one of the neighbours we actually showed
	// the model (any of the top-K), not just the closest one.
	if targetID != "" {
		shown := false
		for _, n := range neighbors {
			if n.ID == targetID {
				shown = true
				break
			}
		}
		if !shown {
			log.Printf("[pipeline] conflict resolver: %s named target %q which is not among this candidate's %d shown neighbour(s) — downgrading to ADD",
				action, previewString(targetID, 64), len(neighbors))
			if action == "UPDATE" || action == "DUPLICATE" {
				action = "ADD"
			}
			return action, ""
		}
	}

	switch action {
	case "UPDATE":
		if targetID != "" {
			return action, targetID // validated against the shown set above
		}
		// Rule 2: an unnamed UPDATE supersedes the NEAREST neighbour only when
		// it's close enough to plausibly BE the same fact.
		if hasNeighbor && neighbors[0].Similarity >= supersedeFloor {
			return action, neighbors[0].ID
		}
		if hasNeighbor {
			log.Printf("[pipeline] conflict resolver: UPDATE with no target and nearest fact at %.2f < %.2f floor — adding instead of superseding",
				neighbors[0].Similarity, supersedeFloor)
		}
		return "ADD", ""
	case "DUPLICATE":
		if !hasNeighbor {
			// Nothing to be a duplicate OF; keep the write.
			return "ADD", ""
		}
		if targetID != "" {
			return action, targetID // validated against the shown set above
		}
		if neighbors[0].Similarity >= supersedeFloor {
			return action, neighbors[0].ID
		}
		log.Printf("[pipeline] conflict resolver: DUPLICATE with no target and nearest fact at %.2f < %.2f floor — keeping the write",
			neighbors[0].Similarity, supersedeFloor)
		return "ADD", ""
	}
	return action, targetID
}

// decisionFor finds the batch decision for candidate i: the one naming i
// as its candidate, else the i-th if that names no candidate. Decisions
// were purely positional, so a model that skipped one shifted the rest:
// candidate [0] took [1]'s DUPLICATE and was dropped.
func decisionFor(i int, decisions []sidecar.BatchDecision) (sidecar.BatchDecision, bool) {
	for _, d := range decisions {
		if d.Candidate != nil && *d.Candidate == i {
			return d, true
		}
	}
	if i < len(decisions) && decisions[i].Candidate == nil {
		return decisions[i], true
	}
	return sidecar.BatchDecision{}, false
}

// recordVersions writes admin-timeline rows for a batch of committed
// facts. Each ADD records "created"; each UPDATE (fact.Supersedes set)
// records "superseded" on the old memory and "created" on the new.
// generated[i], when given, is the id facts[i] had before the commit:
// a fact whose id changed landed on an existing row (a restatement),
// which records "reasserted" rather than "created".
// Errors are logged but never block the write path.
func recordVersions(ctx context.Context, v MemoryVersioner, facts []*pb.FactProto, generated []string) {
	for i, fact := range facts {
		created := "created"
		if i < len(generated) && generated[i] != fact.Id {
			created = "reasserted"
		}
		if fact.Supersedes == "" {
			verCtx, vCancel := context.WithTimeout(ctx, 2*time.Second)
			if err := v.RecordVersion(verCtx, fact.Id, fact.Content,
				fact.Scope, fact.SourceType, "system:extraction", created); err != nil {
				log.Printf("[pipeline] record version (%s) %s: %v", created, fact.Id, err)
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
			fact.Scope, fact.SourceType, "system:extraction", created); err != nil {
			log.Printf("[pipeline] record version (%s-new) %s: %v", created, fact.Id, err)
		}
		vCancel2()
	}
}
