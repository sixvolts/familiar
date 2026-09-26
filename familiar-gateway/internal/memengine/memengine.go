// Package memengine is the in-process replacement for the previous
// engine.
//
// Implements internal/engine.Service against the same shared
// *db.Pool the rest of the gateway uses. Synchronous writes to
// pgvector — no RAM tier, no dirty queue — so a process restart
// can never lose in-flight memory.
//
// Side-by-side with the gRPC engine: main.go picks between Client
// and MemEngine based on [engine] mode. Default stays "grpc" in
// PR-1 so production behavior is unchanged; PR-4 flips the default.
//
// What's not here yet:
//   - Sleep / consolidation runs nothing (PR-3 lands it).
//   - Vault / agent-identity / briefing return synthesized values
//     that satisfy the existing call sites at startup. PR-5 deletes
//     those methods from the Service interface entirely.
package memengine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/db"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/session"
	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MemEngine implements engine.Service in-process. Holds references
// to the shared pool + the optional gateway-side stores it
// delegates to.
//
// memStore is the pgvector-backed read surface (Search / NearestLive
// / NearestSimilarity). When nil — operator hasn't configured
// memory.local_dsn — the memory ops degrade to empty responses
// without erroring, same as the gateway's existing soft-degrade.
//
// sessions is the in-memory turn buffer the gateway already keeps
// per session. AssembleContext reads recent turns from here instead
// of the previous engine's parallel ConversationBuffer.
type MemEngine struct {
	pool     *db.Pool
	memStore memory.MemoryStore
	sessions *session.Manager
	agentID  string

	// startedAt drives the synthetic Ping uptime. Real engine
	// reports the process uptime; in-process reports the memengine
	// instance's lifetime, which is close enough for the health-
	// check banner main.go logs at startup.
	startedAt time.Time

	// sleep is the consolidation goroutine (PR-3). main.go wires it
	// alongside SetDeps and tears it down on shutdown. Nil means
	// the cycle isn't running — StartSleep / SleepStatus still
	// respond with synthetic values so the CLI's start-sleep
	// subcommand doesn't crash.
	sleep *SleepCycle

	// mu protects fields below; everything above is set once at
	// construction.
	mu sync.RWMutex
}

// New constructs a MemEngine from the shared collaborators. agentID
// is the canonical identity main.go uses to tag commits; falls back
// to a hash of the hostname when blank.
func New(pool *db.Pool, memStore memory.MemoryStore, sessions *session.Manager, agentID string) *MemEngine {
	if agentID == "" {
		agentID = "gateway"
	}
	return &MemEngine{
		pool:      pool,
		memStore:  memStore,
		sessions:  sessions,
		agentID:   agentID,
		startedAt: time.Now(),
	}
}

// Close stops the consolidation goroutine it owns (if one was wired
// via SetSleepCycle) and returns. The memengine doesn't own its pool,
// so there's nothing else to release. Defined so the in-process and
// gRPC implementations can be swapped behind the same defer
// eng.Close() in main.go — and so a clean shutdown actually drains the
// sleep cycle instead of killing it mid-pass.
func (e *MemEngine) Close() error {
	e.mu.Lock()
	s := e.sleep
	e.mu.Unlock()
	s.Stop() // nil-safe: no-op when no consolidation cycle was wired
	return nil
}

// SetDeps wires the gateway-side collaborators after construction.
// main.go constructs the memengine at the same boot phase the gRPC
// client used to be dialed — before memStore + sessions exist —
// then calls SetDeps once those are ready. Until then, Ping and
// GetAgentIdentity work (they don't need deps); memory ops degrade
// to "no pool wired" responses, which is fine because nothing on
// the pipeline path runs in the pre-deps window.
func (e *MemEngine) SetDeps(pool *db.Pool, memStore memory.MemoryStore, sessions *session.Manager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pool = pool
	e.memStore = memStore
	e.sessions = sessions
}

// ──────────────────────────────────────────────────────────────────
// Memory ops (real implementations)
// ──────────────────────────────────────────────────────────────────

// AssembleContext returns the conversation history for a turn, read from
// session.RecentTurns.
//
// It no longer performs memory retrieval. The pipeline's own
// searchPgVector (tier-aware hybrid + RRF, effort-resolved thresholds) is
// the single memory authority. This used to ALSO run a dense-only top-3
// @0.70 search whose results were prepended un-reranked ahead of — and
// duplicated by — that hybrid pass, with a fabricated "fresh" staleness;
// that redundant pass is gone. userMsg/vis/queryVec are unused now (the
// method returns conversation history keyed by sessionID) but stay on the
// engine.Service signature; the dead per-tier memBudget/convBudget caps —
// a second budgeting layer atop ctxbuild's window-aware zones — were removed.
func (e *MemEngine) AssembleContext(ctx context.Context, sessionID, userMsg string, vis *pb.VisibilityContext, queryVec []float32) (*pb.AssembleContextResponse, error) {
	out := &pb.AssembleContextResponse{}

	// Conversation branch. Read from session.Manager — the gateway-
	// side source of truth post chat-rearch. Falls back to nil when
	// session isn't wired (test path).
	if e.sessions != nil {
		if sess, ok := e.sessions.Get(sessionID); ok && sess != nil {
			turns := sess.RecentTurns(0) // all turns; pipeline applies its own budget
			for _, t := range turns {
				out.ConversationHistory = append(out.ConversationHistory, &pb.ConversationTurn{
					Role:      t.Role,
					Content:   t.Content,
					Timestamp: timestamppb.New(t.Timestamp),
				})
			}
		}
	}
	return out, nil
}

// CommitFacts writes facts to pgvector synchronously. No RAM tier,
// no dirty queue — every successful return means the row is durable.
//
// A fact whose identity (owner, scope, content; see memory.FactHash)
// matches an existing row lands on that row instead of adding a second
// one: the ON CONFLICT (agent_id, content_hash) upsert. On return each
// committed fact's Id is the id of the row that holds it, which on
// that path is the existing row's id rather than the one the caller
// generated; callers use it for the ids they report, record versions
// against or cite as provenance.
//
// Each fact commits in its own transaction, so one bad fact doesn't
// discard the others, but every failure is reported in the error.
func (e *MemEngine) CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	out := &pb.CommitFactsResponse{}
	if e.pool == nil {
		out.Error = "memengine: no db pool wired"
		return out, fmt.Errorf("memengine: no db pool wired")
	}
	var committed uint32
	var failed []string
	for _, f := range facts {
		if f == nil {
			continue
		}
		c, err := e.commitInTx(ctx, f)
		if err != nil {
			// Single-row failures don't abort the batch: one bad fact should
			// not discard the others. But they MUST reach the caller — see
			// the return below.
			id := f.Id
			out.Error = fmt.Sprintf("commit %s: %v", id, err)
			failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			log.Printf("[memengine] commit fact %s failed: %v", id, err)
			continue
		}
		e.afterCommit(ctx, f, c)
		committed++
	}
	out.Committed = committed

	// Report failure to the caller.
	//
	// This used to return (out, nil) unconditionally, with the reason only
	// on out.Error — a field no caller reads (verified across all five call
	// sites; out.Committed is unread too). So a 5s context expiring on a
	// busy Postgres, a constraint violation, or a malformed `supersedes`
	// UUID made the write vanish behind one internal log line, while the
	// `remember` tool still told the user "Got it, I'll remember that" and
	// save_fact still returned an id that was never inserted. Every call
	// site already branches on err, so returning one is what makes those
	// confirmations honest.
	if len(failed) > 0 {
		return out, fmt.Errorf("memengine: %d of %d fact(s) failed to commit: %s",
			len(failed), len(facts), strings.Join(failed, "; "))
	}
	return out, nil
}

// ReplaceSourceFacts swaps every row a source owns (matched on
// source_type, source_ref and scope_tag) for facts, in one
// transaction: readers see the old set or the new one, never neither,
// and any failure leaves the old set in place. Every fact is stamped
// with the source's type, ref and scope. An empty facts slice just
// clears the source. The wiki knowledge pipeline uses this to re-ingest
// a page; it returns how many old rows were removed.
func (e *MemEngine) ReplaceSourceFacts(ctx context.Context, sessionID, sourceType, sourceRef, scopeTag string, facts []*pb.FactProto) (int64, error) {
	if e.pool == nil {
		return 0, fmt.Errorf("memengine: no db pool wired")
	}
	if sourceType == "" || sourceRef == "" {
		return 0, fmt.Errorf("memengine: replace needs a source type and ref")
	}
	tx, err := e.pool.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		WITH victims AS (
			SELECT id FROM memories
			 WHERE source_type = $1 AND source_ref = $2
			   AND scope_tag IS NOT DISTINCT FROM $3::text
		),
		detach AS (
			UPDATE memories SET supersedes = NULL
			 WHERE supersedes IN (SELECT id FROM victims)
			   AND id NOT IN (SELECT id FROM victims)
		)
		DELETE FROM memories WHERE id IN (SELECT id FROM victims)`,
		sourceType, sourceRef, nullIfEmpty(scopeTag))
	if err != nil {
		return 0, fmt.Errorf("clear %s %s: %w", sourceType, sourceRef, err)
	}
	removed, _ := res.RowsAffected()

	results := make([]commitResult, 0, len(facts))
	var kept []*pb.FactProto
	for _, f := range facts {
		if f == nil {
			continue
		}
		f.SourceType, f.SourceRef, f.ScopeTag = sourceType, sourceRef, scopeTag
		c, err := e.commitOne(ctx, tx, f)
		if err != nil {
			return 0, fmt.Errorf("commit %s: %w", f.Id, err)
		}
		results = append(results, c)
		kept = append(kept, f)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for i, f := range kept {
		e.afterCommit(ctx, f, results[i])
	}
	return removed, nil
}

// commitResult is what one fact's upsert did.
type commitResult struct {
	id         string // the row that holds the fact
	needsEmbed bool   // that row has no vector
	existed    bool   // the fact landed on a row that was already there
}

// commitInTx commits one fact in its own transaction: the upsert and,
// when it lands on an existing row, the chain repair in reassert must
// apply together.
func (e *MemEngine) commitInTx(ctx context.Context, f *pb.FactProto) (commitResult, error) {
	tx, err := e.pool.BeginTx(ctx, nil)
	if err != nil {
		return commitResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := e.commitOne(ctx, tx, f)
	if err != nil {
		return commitResult{}, err
	}
	return c, tx.Commit()
}

// afterCommit does the post-commit bookkeeping: report the stored id
// back through f.Id, and queue a vectorless row for the re-embed sweep.
func (e *MemEngine) afterCommit(ctx context.Context, f *pb.FactProto, c commitResult) {
	f.Id = c.id
	// A fact with no vector is invisible to semantic search, so queue
	// it for the re-embed sweep rather than leaving it half-indexed
	// forever. Best-effort: a failed enqueue must not fail the commit
	// (the fact itself is safely stored and FTS-findable).
	// Conversation facts are never retrieved by vector (every semantic
	// path filters out source_type="conversation"), so a NULL vector is
	// expected here, not a gap to back-fill — enqueuing them would just
	// make the reembed sweeper embed rows nothing reads. Only queue real,
	// retrievable facts.
	if c.needsEmbed && f.SourceType != "conversation" {
		e.enqueuePendingEmbed(ctx, c.id)
	}
}

// commitOne upserts one fact inside tx.
func (e *MemEngine) commitOne(ctx context.Context, tx *sql.Tx, f *pb.FactProto) (commitResult, error) {
	id := f.Id
	if id == "" {
		id = uuid.NewString()
	}
	hash := factHash(f.UserId, f.ScopeTag, f.SourceType, f.SourceRef, f.Content)
	now := time.Now().UTC()
	createdAt := tsOr(f.CreatedAt, now)
	lastAccessed := tsOr(f.LastAccessed, now)
	// RETURNING gives us the id that actually landed (on the ON
	// CONFLICT path that's the pre-existing row, not `id`), whether it
	// ended up without a vector, and whether the row is new (xmax = 0
	// only on a fresh insert). The DO UPDATE doesn't touch `embedding`,
	// so needsEmbed also correctly reports a duplicate arriving against
	// a row that was stored during an earlier embedder outage.
	//
	// The DO UPDATE no longer touches scope_tag: it used to COALESCE
	// the incoming tag onto the existing row, so an isolated shard
	// saving the owner's exact words retagged the owner's top-level fact
	// into the shard's scope. The scope is in the hash now, so a
	// conflict means the same scope; the WHERE refuses (no row comes
	// back) a legacy row whose hash predates that and whose owner or
	// scope differs.
	var c commitResult
	var inserted bool
	err := tx.QueryRowContext(ctx, `
		INSERT INTO memories (
			id, agent_id, scope, content, content_hash, embedding,
			source_type, source_ref, source_description,
			confidence, confidence_basis,
			created_at, updated_at, last_accessed, access_count,
			tags, supersedes, user_id, scope_tag
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9,
			$10, $11,
			$12, NOW(), $13, $14,
			$15, $16, $17, $18
		)
		ON CONFLICT (agent_id, content_hash) DO UPDATE SET
			updated_at    = NOW(),
			last_accessed = GREATEST(memories.last_accessed, EXCLUDED.last_accessed),
			access_count  = memories.access_count + 1
		WHERE memories.user_id IS NOT DISTINCT FROM EXCLUDED.user_id
		  AND memories.scope_tag IS NOT DISTINCT FROM EXCLUDED.scope_tag
		RETURNING id::text, (embedding IS NULL), (xmax = 0)`,
		id, e.agentID, scopeOr(f.Scope, "session"), f.Content, hash, vectorParam(f.Embedding),
		f.SourceType, f.SourceRef, f.SourceDescription,
		float64(f.Confidence), f.ConfidenceBasis,
		createdAt, lastAccessed, int(f.AccessCount),
		tagsParam(f.Tags), nullIfEmpty(f.Supersedes), nullIfEmpty(f.UserId), nullIfEmpty(f.ScopeTag),
	).Scan(&c.id, &c.needsEmbed, &inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return commitResult{}, fmt.Errorf("content matches an existing memory with a different owner or scope")
	}
	if err != nil {
		return commitResult{}, err
	}
	c.existed = !inserted
	if c.existed {
		if err := reassert(ctx, tx, c.id, f.Supersedes); err != nil {
			return commitResult{}, fmt.Errorf("reassert %s: %w", c.id, err)
		}
	}
	return c, nil
}

// reassert handles a fact that was stated again and landed on its
// existing row x. Before this, the upsert only bumped x's counters:
//
//   - x stayed hidden if a newer row had superseded it. "I live in
//     Portland", then "I moved to Seattle", then "I moved back to
//     Portland" left Seattle live and Portland hidden for good.
//   - the new statement's own supersedes pointer was dropped, so the
//     fact it was meant to replace stayed live next to it.
//
// So a re-statement moves x back to the head of its chain: the rows
// that superseded x now supersede x's predecessor instead (so the older
// history stays hidden), and x takes the incoming supersedes target.
func reassert(ctx context.Context, tx *sql.Tx, x, target string) error {
	var prev sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT supersedes::text FROM memories WHERE id = $1::uuid FOR UPDATE`, x,
	).Scan(&prev); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE memories SET supersedes = $2::uuid, updated_at = NOW() WHERE supersedes = $1::uuid`,
		x, prev)
	if err != nil {
		return err
	}
	hadChildren, _ := res.RowsAffected()

	if target == "" || target == x {
		return nil
	}
	// A target that is already older than x in its own chain is hidden
	// by it already; pointing x at it would unhide the rows in between.
	var older bool
	if err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE older AS (
			SELECT supersedes FROM memories WHERE id = $1::uuid
			UNION
			SELECT m.supersedes FROM memories m JOIN older o ON m.id = o.supersedes
		)
		SELECT EXISTS (SELECT 1 FROM older WHERE supersedes = $2::uuid)`, x, target,
	).Scan(&older); err != nil {
		return err
	}
	if older {
		return nil
	}
	// x can hold one pointer. If it alone was hiding prev, hand prev to
	// the target first (x -> target -> prev); if the target already has
	// history of its own, leave x's chain alone rather than unhide prev.
	if prev.Valid && hadChildren == 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE memories SET supersedes = $2::uuid WHERE id = $1::uuid AND supersedes IS NULL`,
			target, prev.String)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE memories SET supersedes = $2::uuid WHERE id = $1::uuid`, x, target)
	return err
}

// enqueuePendingEmbed records a memory as awaiting an embedding. Called
// when a commit lands with a NULL vector (no embedder reachable). The PK
// on memory_id makes this idempotent, so re-committing the same content
// during a long outage doesn't pile up rows. A row that is already
// queued gets its rejection count reset: it was just written again (an
// edit, or a restatement), so a row the sweep had parked gets another
// round of tries.
func (e *MemEngine) enqueuePendingEmbed(ctx context.Context, memoryID string) {
	if e.pool == nil || memoryID == "" {
		return
	}
	if _, err := e.pool.ExecContext(ctx, `
		INSERT INTO pending_embeds (memory_id) VALUES ($1::uuid)
		ON CONFLICT (memory_id) DO UPDATE SET attempts = 0`, memoryID); err != nil {
		log.Printf("[memengine] warning: could not queue memory %s for re-embed: %v", memoryID, err)
	}
}

// DeleteFact removes a memory row by id, along with the older versions
// it replaced; rows that pointed at any of them are detached. Same
// statement as memory.DeleteMemory — see memory.DeleteVersionsSQL for
// why the older versions have to go too.
func (e *MemEngine) DeleteFact(ctx context.Context, sessionID, factID string, vis *pb.VisibilityContext) (*pb.DeleteFactResponse, error) {
	out := &pb.DeleteFactResponse{}
	if e.pool == nil {
		out.Error = "memengine: no db pool wired"
		return out, nil
	}
	if factID == "" {
		out.Error = "fact_id required"
		return out, nil
	}
	res, err := e.pool.ExecContext(ctx, memory.DeleteVersionsSQL, factID, nil)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	n, _ := res.RowsAffected()
	out.Deleted = n > 0
	return out, nil
}

// UpdateFact replaces a memory's content + embedding in place, and
// records the change in memory_versions (seeding the original text as
// version 1 when the row has no history yet), all in one transaction.
// It used to overwrite the row with no version at all, so a
// correct_fact that picked the wrong row destroyed that row's text
// with no way back.
func (e *MemEngine) UpdateFact(ctx context.Context, sessionID, factID, newContent string, newEmbedding []float32, vis *pb.VisibilityContext) (*pb.UpdateFactResponse, error) {
	out := &pb.UpdateFactResponse{}
	if e.pool == nil {
		out.Error = "memengine: no db pool wired"
		return out, nil
	}
	if factID == "" {
		out.Error = "fact_id required"
		return out, nil
	}
	updated, err := e.updateFact(ctx, factID, newContent, newEmbedding, "user:"+vis.GetUserId())
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	out.Updated = updated
	// An edit whose re-embed failed (embedder down) just cleared this
	// row's vector, so queue it for the sweep — otherwise the edited text
	// stays out of semantic search until someone edits it again.
	if out.Updated && len(newEmbedding) == 0 {
		e.enqueuePendingEmbed(ctx, factID)
	}
	return out, nil
}

func (e *MemEngine) updateFact(ctx context.Context, factID, newContent string, newEmbedding []float32, changedBy string) (bool, error) {
	tx, err := e.pool.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var content, scope, sourceType, sourceRef, userID, scopeTag string
	err = tx.QueryRowContext(ctx, `
		SELECT content, scope, COALESCE(source_type, ''), COALESCE(source_ref, ''),
		       COALESCE(user_id, ''), COALESCE(scope_tag, '')
		  FROM memories WHERE id = $1::uuid FOR UPDATE`, factID,
	).Scan(&content, &scope, &sourceType, &sourceRef, &userID, &scopeTag)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	record := func(text, by, change string) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO memory_versions (memory_id, content, scope, source_type, version, changed_by, change_type)
			SELECT $1::uuid, $2, $3, $4,
			       COALESCE((SELECT MAX(version) FROM memory_versions WHERE memory_id = $1::uuid), 0) + 1,
			       $5, $6`,
			factID, text, scope, sourceType, by, change)
		return err
	}
	var versions int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_versions WHERE memory_id = $1::uuid`, factID,
	).Scan(&versions); err != nil {
		return false, err
	}
	if versions == 0 {
		if err := record(content, "", "created"); err != nil {
			return false, err
		}
	}
	// Hash from the row's own owner and scope, so a later commit of the
	// same content by the same owner in the same scope dedups onto it.
	hash := factHash(userID, scopeTag, sourceType, sourceRef, newContent)
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		   SET content       = $2,
		       content_hash  = $3,
		       embedding     = $4,
		       updated_at    = NOW()
		 WHERE id = $1::uuid`, factID, newContent, hash, vectorParam(newEmbedding)); err != nil {
		return false, err
	}
	if err := record(newContent, changedBy, "updated"); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// QueryMemory is the legacy admin / CLI / memory-skill retrieval
// surface. Wraps memStore.Search in the proto Result shape so
// existing callers keep working without refactor. PR-5 deletes the
// proto wrapping and switches callers to memStore.Search directly.
func (e *MemEngine) QueryMemory(ctx context.Context, req *pb.MemoryQueryRequest) (*pb.MemoryQueryResponse, error) {
	out := &pb.MemoryQueryResponse{}
	if e.memStore == nil || req == nil {
		return out, nil
	}
	// MemoryQueryRequest is a oneof; only the Semantic branch maps
	// cleanly to memStore.Search. Other variants (entity / relational
	// / temporal / hybrid) aren't reachable in the live gateway —
	// they're dead code. Falls through to empty.
	sem := req.GetSemantic()
	if sem == nil {
		return out, nil
	}
	userID := ""
	if sem.Visibility != nil {
		userID = sem.Visibility.UserId
	}
	limit := int(sem.Limit)
	if limit <= 0 {
		limit = 10
	}
	results, err := e.memStore.Search(ctx, sem.QueryVector, limit, 0.0, userID)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	for _, r := range results {
		out.Results = append(out.Results, &pb.MemoryResultProto{
			Fact: &pb.FactProto{
				Id:        r.ID,
				Content:   r.Content,
				Embedding: r.Embedding,
				Scope:     r.Scope,
			},
			RelevanceScore: float32(r.Similarity),
			TierSource:     "persistent",
		})
	}
	return out, nil
}

// SetSleepCycle wires the consolidation goroutine. Once set, the
// StartSleep / SleepStatus RPCs trigger a real on-demand pass and
// surface the most recent cycle's stats. Nil keeps the synthetic
// no-op behavior from PR-1.
func (e *MemEngine) SetSleepCycle(s *SleepCycle) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sleep = s
}

// ──────────────────────────────────────────────────────────────────
// Synthetic stubs (deleted in PR-5)
// ──────────────────────────────────────────────────────────────────

// Ping returns synthesized health info matching the previous engine's response shape.
// version reports "memengine/in-process" so an operator can tell at
// a glance which path is live. memory_tier is "pgvector" — there's
// no RAM tier here by design.
func (e *MemEngine) Ping(ctx context.Context) (*pb.PingResponse, error) {
	return &pb.PingResponse{
		Version:    "memengine/in-process",
		UptimeSecs: uint64(time.Since(e.startedAt).Seconds()),
		MemoryTier: "pgvector",
	}, nil
}

// GetAgentIdentity returns the agent id plus a stable fingerprint
// derived from the agent id. The previous implementation computes the fingerprint
// from a vault-stored keypair; the in-process gateway doesn't own
// crypto keys, so the fingerprint is just a SHA-256 prefix.
func (e *MemEngine) GetAgentIdentity(ctx context.Context) (*pb.AgentIdentityResponse, error) {
	sum := sha256.Sum256([]byte("familiar:agent:" + e.agentID))
	return &pb.AgentIdentityResponse{
		AgentId:     e.agentID,
		Fingerprint: hex.EncodeToString(sum[:8]),
	}, nil
}

// GetBriefing is vestigial — only the CLI's `briefing` subcommand
// reads it, and the previous implementation returns a hardcoded summary. Mirror
// that here so the subcommand keeps working until PR-5 drops it.
func (e *MemEngine) GetBriefing(ctx context.Context) (*pb.BriefingResponse, error) {
	return &pb.BriefingResponse{
		Summary: "Familiar in-process engine. No briefing surface in this build.",
	}, nil
}

// VaultGet / VaultSet are dead — no live caller in the gateway.
// Returning ErrUnsupported here matches the previous implementation's "vault
// disabled" branch and lets us delete the methods entirely in PR-5
// without surprising a runtime caller in between.
func (e *MemEngine) VaultGet(ctx context.Context, key string) (string, bool, error) {
	return "", false, ErrUnsupported
}
func (e *MemEngine) VaultSet(ctx context.Context, key, value string) error {
	return ErrUnsupported
}

// StartSleep triggers an on-demand consolidation pass when the
// cycle is wired (PR-3); otherwise returns a synthetic no-op
// handle. phases is ignored — the previous implementation honored it for partial
// phase selection but no caller in the gateway uses anything other
// than the all-phases default.
func (e *MemEngine) StartSleep(ctx context.Context, phases []string) (string, error) {
	if e.sleep == nil {
		return "memengine-no-op", nil
	}
	// Fire-and-forget so the CLI's start-sleep returns promptly.
	// The handle just names "the most recent cycle" — SleepStatus
	// reads sleep.LastStats() regardless of which handle the CLI
	// supplies.
	go e.sleep.RunOnce(context.Background())
	return "memengine-cycle", nil
}

// SleepStatus reports the most recent cycle's stats. Returns
// "idle" + completed=true when no cycle has run yet (or the cycle
// isn't wired).
func (e *MemEngine) SleepStatus(ctx context.Context, handle string) (*pb.SleepStatusResponse, error) {
	if e.sleep == nil {
		return &pb.SleepStatusResponse{Phase: "idle", Completed: true}, nil
	}
	last := e.sleep.LastStats()
	if last.StartedAt.IsZero() {
		return &pb.SleepStatusResponse{Phase: "idle", Completed: true}, nil
	}
	return &pb.SleepStatusResponse{
		Phase:     "done",
		Progress:  1.0,
		Completed: true,
	}, nil
}

// WakeSleep stops the consolidation goroutine. main.go normally
// handles teardown via the shutdown ctx, but the RPC stays for
// parity with the previous engine's surface.
func (e *MemEngine) WakeSleep(ctx context.Context, handle string) error {
	if e.sleep != nil {
		e.sleep.Stop()
	}
	return nil
}

// ErrUnsupported is returned by the vault stubs. Exposed for the
// rare caller (admin tooling) that wants to differentiate "feature
// off in this build" from a real error.
var ErrUnsupported = errors.New("memengine: feature not available in in-process mode")

// ──────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// factHash is the content_hash behind the (agent_id, content_hash)
// write-time dedup; see memory.FactHash for what it covers and why.
func factHash(userID, scopeTag, sourceType, sourceRef, content string) string {
	return memory.FactHash(userID, scopeTag, sourceType, sourceRef, content)
}

// nullIfEmpty maps "" to SQL NULL for optional text and uuid columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func tsOr(t *timestamppb.Timestamp, fallback time.Time) time.Time {
	if t == nil {
		return fallback
	}
	return t.AsTime()
}

func scopeOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// vectorParam renders a []float32 into the pgvector text format
// "[v1,v2,...]". Returns NULL when the embedding is empty so the
// column accepts it. Mirrors the gateway-side existing pgvector
// helpers.
func vectorParam(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
}

// tagsParam returns the tags slice in a form pgx will marshal as
// TEXT[]. The column is NOT NULL DEFAULT '{}' — an explicit NULL
// bypasses the default and violates the constraint, so an empty
// slice must go over the wire as '{}', not NULL.
func tagsParam(tags []string) any {
	if len(tags) == 0 {
		return []string{}
	}
	return tags
}

// pgArray wraps a []string for pgx so the ANY($1::uuid[]) cast
// works without per-row binding. pgx already understands []string
// → text[] / uuid[], so the direct slice is correct.
func pgArray(ids []string) any { return ids }
