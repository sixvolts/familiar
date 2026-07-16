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

// AssembleContext is the per-turn retrieval call. Returns the same
// shape the previous engine returned, built from:
//   - pgvector search (memStore) for the memory_context list
//   - session.RecentTurns for the conversation_history list
//
// The hardcoded threshold (0.70) + top-K (3) from the previous implementation are
// kept here for byte-for-byte parity with the existing pipeline
// behavior. The effort resolver will override these via
// memStore.Search at the pipeline's own retrieval call site;
// AssembleContext stays conservative since it's still the legacy
// surface.
func (e *MemEngine) AssembleContext(ctx context.Context, sessionID, userMsg string, vis *pb.VisibilityContext, memBudget, convBudget uint32, queryVec []float32) (*pb.AssembleContextResponse, error) {
	out := &pb.AssembleContextResponse{}
	// Memory branch. Skip when the operator hasn't wired memStore
	// or when the caller didn't supply a query vector — both are
	// soft-fail conditions matching the previous engine's behavior.
	if e.memStore != nil && len(queryVec) > 0 {
		userID := ""
		if vis != nil {
			userID = vis.UserId
		}
		const (
			minRelevance = 0.70
			maxInjected  = 10
		)
		results, err := e.memStore.Search(ctx, queryVec, maxInjected, minRelevance, userID)
		if err != nil {
			out.Error = "memory query failed: " + err.Error()
			return out, nil
		}
		// Cap to top-3 above threshold to match the previous implementation's
		// "max_injected = 3" constant. Search() already orders by
		// similarity desc.
		take := 3
		if take > len(results) {
			take = len(results)
		}
		for _, r := range results[:take] {
			out.MemoryContext = append(out.MemoryContext, &pb.MemoryResultProto{
				Fact: &pb.FactProto{
					Id:        r.ID,
					Content:   r.Content,
					Embedding: r.Embedding,
					Scope:     r.Scope,
				},
				RelevanceScore:    float32(r.Similarity),
				TierSource:        "persistent",
				ProvenanceSummary: r.Scope,
				Staleness:         "fresh",
			})
		}
	}

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
// Mirrors the previous engine's ON CONFLICT (agent_id, content_hash)
// upsert behavior so a re-commit of the same content is idempotent.
func (e *MemEngine) CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error) {
	out := &pb.CommitFactsResponse{}
	if e.pool == nil {
		out.Error = "memengine: no db pool wired"
		return out, nil
	}
	var committed uint32
	for _, f := range facts {
		if f == nil {
			continue
		}
		id := f.Id
		if id == "" {
			id = uuid.NewString()
		}
		hash := factHash(f.UserId, f.Content)
		now := time.Now().UTC()
		createdAt := tsOr(f.CreatedAt, now)
		lastAccessed := tsOr(f.LastAccessed, now)
		var supersedes any
		if f.Supersedes != "" {
			supersedes = f.Supersedes
		}
		var scopeTag any
		if f.ScopeTag != "" {
			scopeTag = f.ScopeTag
		}
		var userID any
		if f.UserId != "" {
			userID = f.UserId
		}
		_, err := e.pool.ExecContext(ctx, `
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
				access_count  = memories.access_count + 1,
				scope_tag     = COALESCE(memories.scope_tag, EXCLUDED.scope_tag)`,
			id, e.agentID, scopeOr(f.Scope, "session"), f.Content, hash, vectorParam(f.Embedding),
			f.SourceType, f.SourceRef, f.SourceDescription,
			float64(f.Confidence), f.ConfidenceBasis,
			createdAt, lastAccessed, int(f.AccessCount),
			tagsParam(f.Tags), supersedes, userID, scopeTag)
		if err != nil {
			// Single-row failures don't abort the batch — same as
			// the previous implementation which logs and continues. Surface the
			// last error on the response so the gateway logs it.
			out.Error = fmt.Sprintf("commit %s: %v", id, err)
			log.Printf("[memengine] commit fact %s failed: %v", id, err)
			continue
		}
		committed++
	}
	out.Committed = committed
	return out, nil
}

// DeleteFact removes a memory row by id. Mirrors the previous implementation's
// soft-vs-hard semantics: if the row has dependents (children
// pointing at it via supersedes), we just clear the dependents and
// then delete. The gateway uses this from memory-skill paths only.
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
	// Detach children, then delete. Done in one round-trip via CTE
	// so a concurrent insert can't slot in between.
	res, err := e.pool.ExecContext(ctx, `
		WITH detach AS (
			UPDATE memories SET supersedes = NULL WHERE supersedes = $1
		)
		DELETE FROM memories WHERE id = $1`, factID)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	n, _ := res.RowsAffected()
	out.Deleted = n > 0
	return out, nil
}

// UpdateFact replaces a memory's content + embedding in place. The
// memory-skill update path is the only caller; the row's history
// stays in wiki_revisions if applicable but in this table the
// previous content is overwritten.
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
	// Keep the owner in the hash consistent with CommitFacts so a later
	// commit of the same content by the same user still dedups onto
	// this row. vis carries the acting user (nil-safe).
	hash := factHash(vis.GetUserId(), newContent)
	res, err := e.pool.ExecContext(ctx, `
		UPDATE memories
		   SET content       = $2,
		       content_hash  = $3,
		       embedding     = $4,
		       updated_at    = NOW()
		 WHERE id = $1`, factID, newContent, hash, vectorParam(newEmbedding))
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	n, _ := res.RowsAffected()
	out.Updated = n > 0
	return out, nil
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

// factHash is the content_hash used for the (agent_id, content_hash)
// write-time dedup. It folds the owner into the hash so byte-identical
// content from two DIFFERENT users produces different hashes and can't
// collide onto one row — the cross-tenant dedup bug where user B's
// fact silently became an access-count bump on user A's row. Global
// rows (userID == "") share the empty prefix, so they still dedup
// among themselves exactly as before. The NUL separator keeps
// (user="ab", content="c") from hashing the same as (user="a",
// content="bc").
func factHash(userID, content string) string {
	return sha256Hex(userID + "\x00" + content)
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
