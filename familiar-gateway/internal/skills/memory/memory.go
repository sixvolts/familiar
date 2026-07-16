// Package memory provides the Familiar memory skill: LLM-callable tools
// for persisting long-term facts and searching them semantically.
//
// This skill is the writable counterpart to the read-side memory
// injection the pipeline already performs during context assembly.
// Where the pipeline implicitly retrieves relevant memories on every
// turn, this skill lets the model explicitly say "remember this" or
// "what do I know about X".
//
// Dependencies are all passed in through New so the package stays
// agnostic of the wider gateway wiring (no imports of pipeline, router,
// config, etc.). Any of engine / store / profiles / embed may be nil;
// the matching tools simply return an error when invoked without their
// backend.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/familiar/gateway/internal/engine"
	mem "github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/skills"
	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EmbedFunc computes a dense vector for a text string. Matches the
// pipeline's EmbedFunc signature intentionally — callers typically pass
// the same function they hand to the pipeline — but we redeclare it
// here so the skill package doesn't have to import pipeline and create
// an import cycle.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// MemoryManager is the write/admin surface of the memory store that
// the user-facing tools need but the retrieval-only MemoryStore
// interface doesn't expose. PgVectorStore satisfies this directly.
type MemoryManager interface {
	ListMemories(ctx context.Context, f mem.MemoryFilter, limit, offset int) ([]mem.MemoryRow, error)
	// DeleteMemoryOwned deletes a row only if it belongs to userID,
	// reporting whether a row was actually removed. The chat memory
	// tools run on behalf of one user, so the unscoped DeleteMemory is
	// deliberately not on this interface — see forget_fact.
	DeleteMemoryOwned(ctx context.Context, id, userID string) (bool, error)
	UpdateMemoryContent(ctx context.Context, id, newContent, changedBy string, embedding []float32) error
}

// Skill exposes save_fact, remember, search_memory, and the
// memory-management tools (list / forget / correct).
type Skill struct {
	engine  engine.Service
	store   mem.MemoryStore
	manager MemoryManager
	embed   EmbedFunc
}

// New constructs the memory skill. Any dependency may be nil, which
// disables the tools that require it (they return a user-facing error
// at Execute time). The embed function is the only dependency that has
// no graceful fallback for save_fact — without embeddings, saved facts
// are unreachable through semantic search — so callers are expected to
// wire the same embedder the pipeline uses.
// Option functions for optional dependencies.
type Option func(*Skill)

// WithManager attaches the write/admin memory surface needed by
// forget_fact, list_my_memories, and correct_fact. Without it those
// tools return a user-facing error; the core tools (save_fact,
// search_memory, etc.) still work fine.
func WithManager(m MemoryManager) Option {
	return func(s *Skill) { s.manager = m }
}

func New(eng engine.Service, store mem.MemoryStore, embed EmbedFunc, opts ...Option) *Skill {
	s := &Skill{
		engine: eng,
		store:  store,
		embed:  embed,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Skill) Name() string                 { return "memory" }
func (s *Skill) Description() string          { return "Persistent memory: save facts and search them" }
func (s *Skill) Version() string              { return "1.0.0" }
func (s *Skill) Init(_ json.RawMessage) error { return nil }
func (s *Skill) Close() error                 { return nil }

var saveFactParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "content": {
      "type": "string",
      "description": "The fact to remember. Write a single self-contained statement — future retrieval embeds this verbatim."
    },
    "scope": {
      "type": "string",
      "description": "Visibility scope. \"user\" = durable knowledge about the user (default). \"session\" = only this conversation. \"agent\" = private to the assistant.",
      "enum": ["user", "session", "agent"]
    },
    "tags": {
      "type": "array",
      "description": "Optional topical tags for later filtering.",
      "items": {"type": "string"}
    },
    "confidence": {
      "type": "number",
      "description": "How sure you are (0.0-1.0). Defaults to 0.9.",
      "minimum": 0,
      "maximum": 1
    }
  },
  "required": ["content"]
}`)

var rememberParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "content": {
      "type": "string",
      "description": "What to remember, written as a clear self-contained factual statement. Use when the user explicitly says \"remember this\", \"don't forget\", or asks you to store something."
    },
    "importance": {
      "type": "string",
      "description": "Retrieval priority hint. \"high\" for things the user stressed, \"medium\" (default) for normal asks, \"low\" for passing mentions.",
      "enum": ["high", "medium", "low"]
    }
  },
  "required": ["content"]
}`)

var searchMemoryParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Natural-language query. Example: \"user's preferred editor\" or \"past decisions about the Slack adapter\"."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum results to return (1-20). Defaults to 5.",
      "minimum": 1,
      "maximum": 20
    },
    "threshold": {
      "type": "number",
      "description": "Minimum cosine similarity (0.0-1.0). Defaults to 0.65.",
      "minimum": 0,
      "maximum": 1
    }
  },
  "required": ["query"]
}`)

var listMyMemoriesParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "description": "Maximum results (1-50). Defaults to 10.",
      "minimum": 1,
      "maximum": 50
    },
    "offset": {
      "type": "integer",
      "description": "Pagination offset. Defaults to 0.",
      "minimum": 0
    },
    "query": {
      "type": "string",
      "description": "Optional content substring filter."
    }
  }
}`)

var forgetFactParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Description of the memory to forget. Searched by semantic similarity to find the best match."
    },
    "id": {
      "type": "string",
      "description": "Exact memory ID to delete. If provided, query is ignored."
    }
  }
}`)

var correctFactParams = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Description of the memory to correct. Searched by semantic similarity to find the best match."
    },
    "new_content": {
      "type": "string",
      "description": "The corrected fact to replace the old content with."
    }
  },
  "required": ["query", "new_content"]
}`)

func (s *Skill) Tools() []skills.ToolDefinition {
	return []skills.ToolDefinition{
		{
			Name:        "save_fact",
			Description: "Persist a fact to long-term memory. Use when you learn something worth remembering across conversations.",
			Parameters:  saveFactParams,
		},
		{
			Name:        "remember",
			Description: "Explicitly store something the user asked you to remember. Use ONLY when the user directly requests it (\"remember this\", \"don't forget\"); for proactive extraction use save_fact. Stores at confidence 1.0 with source_type=explicit.",
			Parameters:  rememberParams,
		},
		{
			Name:        "search_memory",
			Description: "Semantic search over stored memories. Returns relevant facts with similarity scores.",
			Parameters:  searchMemoryParams,
		},
		{
			Name:        "list_my_memories",
			Description: "List the current user's recent memories. Use when the user asks \"what do you remember about me?\" or wants to browse their stored facts.",
			Parameters:  listMyMemoriesParams,
		},
		{
			Name:        "forget_fact",
			Description: "Delete a memory. Use when the user asks you to forget something. Finds the closest match by semantic similarity or accepts an exact ID.",
			Parameters:  forgetFactParams,
		},
		{
			Name:        "correct_fact",
			Description: "Update a memory with corrected content. Use when the user says something you remember is wrong and provides the correction.",
			Parameters:  correctFactParams,
		},
	}
}

func (s *Skill) Execute(ctx context.Context, toolName string, params json.RawMessage) (skills.ToolResult, error) {
	switch toolName {
	case "save_fact":
		return s.execSaveFact(ctx, params)
	case "remember":
		return s.execRemember(ctx, params)
	case "search_memory":
		return s.execSearchMemory(ctx, params)
	case "list_my_memories":
		return s.execListMyMemories(ctx, params)
	case "forget_fact":
		return s.execForgetFact(ctx, params)
	case "correct_fact":
		return s.execCorrectFact(ctx, params)
	default:
		return skills.ToolResult{}, fmt.Errorf("memory: unknown tool %q", toolName)
	}
}

// --- save_fact --------------------------------------------------------------

type saveFactArgs struct {
	Content    string   `json:"content"`
	Scope      string   `json:"scope,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

func (s *Skill) execSaveFact(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	if s.engine == nil {
		return skills.ToolResult{Error: "memory: engine unavailable"}, nil
	}

	var args saveFactArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if strings.TrimSpace(args.Content) == "" {
		return skills.ToolResult{Error: "content is required"}, nil
	}
	scope := args.Scope
	if scope == "" {
		scope = "user"
	}
	conf := 0.9
	if args.Confidence != nil {
		conf = *args.Confidence
	}

	// Embed the content so it's reachable via search_memory. Missing
	// embedder is not fatal — the fact still lands in the store, just
	// without a vector to retrieve it by. We log that by returning a
	// note in Content so the model knows.
	var vec []float32
	if s.embed != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		v, err := s.embed(embedCtx, args.Content)
		cancel()
		if err == nil {
			vec = v
		}
	}

	sc, _ := skills.ContextFrom(ctx)
	sessionID := sc.SessionID
	if sessionID == "" {
		sessionID = "skill-memory"
	}

	now := time.Now()
	userID := sc.UserID
	if userID == "" {
		// OWNER-MIGRATION: refuse to attribute memory writes/reads to
		// the legacy "owner" canonical_id when the skill context is
		// missing a user. A blank UserID at this layer means the
		// caller never resolved identity (pipeline.resolveIdentity hit
		// the unmapped path) — the memory tools must refuse rather
		// than silently scope everything to the bootstrap admin.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}
	fact := &pb.FactProto{
		Id:           uuid.NewString(),
		Content:      args.Content,
		Embedding:    vec,
		SourceType:   "skill:memory",
		Confidence:   float32(conf),
		Scope:        scope,
		Tags:         args.Tags,
		CreatedAt:    timestamppb.New(now),
		LastAccessed: timestamppb.New(now),
		UserId:       userID,
		// Shard scope — empty for trusted-path calls, non-empty when
		// save_fact runs inside a persistent shard's tool loop. The
		// pipeline populates sc.ScopeTag at the skills.SessionContext
		// boundary (see pipeline.runCompletion).
		ScopeTag: sc.ScopeTag,
		// ExcludeFromHot mirrors the FactProto field — set by the
		// pipeline only when running an isolated-visibility shard, so
		// the engine writes this fact past its RAM cache straight to
		// pgvector (FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3).
		ExcludeFromHot: sc.ExcludeFromHot,
	}

	commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.engine.CommitFacts(commitCtx, sessionID, []*pb.FactProto{fact}); err != nil {
		return skills.ToolResult{}, fmt.Errorf("commit fact: %w", err)
	}

	note := ""
	if vec == nil {
		note = " (note: no embedding — unreachable via semantic search)"
	}
	content := fmt.Sprintf("Saved fact (scope=%s, id=%s)%s", scope, fact.Id, note)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

// --- remember ---------------------------------------------------------------

type rememberArgs struct {
	Content    string `json:"content"`
	Importance string `json:"importance,omitempty"`
}

// execRemember is the user-triggered sibling of save_fact. Where
// save_fact is the model's proactive "I noticed something worth
// keeping" path, remember is reserved for when the user has directly
// asked for storage. It stores at full confidence with
// source_type=explicit so the sleep cycle's decay model treats it as
// load-bearing and won't prune it under normal eviction.
func (s *Skill) execRemember(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	if s.engine == nil {
		return skills.ToolResult{Error: "memory: engine unavailable"}, nil
	}

	var args rememberArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if strings.TrimSpace(args.Content) == "" {
		return skills.ToolResult{Error: "content is required"}, nil
	}

	importance := strings.ToLower(strings.TrimSpace(args.Importance))
	switch importance {
	case "", "medium":
		importance = "medium"
	case "high", "low":
	default:
		return skills.ToolResult{Error: "importance must be high, medium, or low"}, nil
	}

	var vec []float32
	if s.embed != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		v, err := s.embed(embedCtx, args.Content)
		cancel()
		if err == nil {
			vec = v
		}
	}

	sc, _ := skills.ContextFrom(ctx)
	sessionID := sc.SessionID
	if sessionID == "" {
		sessionID = "skill-memory"
	}
	userID := sc.UserID
	if userID == "" {
		// OWNER-MIGRATION: refuse to attribute memory writes/reads to
		// the legacy "owner" canonical_id when the skill context is
		// missing a user. A blank UserID at this layer means the
		// caller never resolved identity (pipeline.resolveIdentity hit
		// the unmapped path) — the memory tools must refuse rather
		// than silently scope everything to the bootstrap admin.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}

	now := time.Now()
	fact := &pb.FactProto{
		Id:                uuid.NewString(),
		Content:           args.Content,
		Embedding:         vec,
		SourceType:        "explicit",
		SourceDescription: "User asked to remember this",
		Confidence:        1.0,
		ConfidenceBasis:   "user_stated",
		Scope:             "user",
		UserId:            userID,
		Tags:              []string{"importance:" + importance},
		CreatedAt:         timestamppb.New(now),
		LastAccessed:      timestamppb.New(now),
		// See execSaveFact above for the shard-scoping rationale.
		ScopeTag:       sc.ScopeTag,
		ExcludeFromHot: sc.ExcludeFromHot,
	}

	commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.engine.CommitFacts(commitCtx, sessionID, []*pb.FactProto{fact}); err != nil {
		return skills.ToolResult{}, fmt.Errorf("commit fact: %w", err)
	}

	content := "Got it, I'll remember that."
	if vec == nil {
		content += " (note: no embedding — unreachable via semantic search)"
	}
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

// --- engine-backed semantic search helper -----------------------------------

// engineSemanticSearch runs a semantic query through the engine
// (memengine, which queries pgvector). Falls back to the local
// pgvector store if the engine is unavailable. The engine migration
// PR-1 collapsed the old "tier-merged (RAM + pgvector)" semantics —
// there's a single tier now, so the engine path and the store path
// see the same rows.
func (s *Skill) engineSemanticSearch(ctx context.Context, vec []float32, limit int, userID string) ([]*pb.MemoryResultProto, error) {
	if s.engine == nil {
		return nil, fmt.Errorf("engine unavailable")
	}
	vis := &pb.VisibilityContext{UserId: userID}
	req := &pb.MemoryQueryRequest{
		Query: &pb.MemoryQueryRequest_Semantic{
			Semantic: &pb.SemanticQuery{
				QueryVector: vec,
				Limit:       uint32(limit),
				Scope:       "user",
				Visibility:  vis,
			},
		},
	}
	qCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.engine.QueryMemory(qCtx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("engine: %s", resp.Error)
	}
	return resp.Results, nil
}

// --- search_memory ----------------------------------------------------------

type searchMemoryArgs struct {
	Query     string   `json:"query"`
	Limit     *int     `json:"limit,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

func (s *Skill) execSearchMemory(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	if s.embed == nil {
		return skills.ToolResult{Error: "memory: embedder unavailable"}, nil
	}

	var args searchMemoryArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	if strings.TrimSpace(args.Query) == "" {
		return skills.ToolResult{Error: "query is required"}, nil
	}
	limit := 5
	if args.Limit != nil && *args.Limit > 0 {
		limit = *args.Limit
	}
	threshold := 0.65
	if args.Threshold != nil {
		threshold = *args.Threshold
	}

	embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	vec, err := s.embed(embedCtx, args.Query)
	cancel()
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("embed query: %w", err)
	}

	var userID string
	if sc, ok := skills.ContextFrom(ctx); ok {
		userID = sc.UserID
	}
	if userID == "" {
		// OWNER-MIGRATION: search_memory used to scope to "owner"
		// implicitly when the skill context lacked a user. That meant
		// every unauthenticated caller could read Operator's facts.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}

	// Prefer the engine's tier-merged query (RAM + pgvector) so fresh
	// writes from remember/save_fact are immediately visible. Fall back
	// to the local pgvector store if the engine is unavailable.
	results, err := s.engineSemanticSearch(ctx, vec, limit, userID)
	if err == nil && len(results) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Memory search results for %q:\n", args.Query)
		shown := 0
		for _, r := range results {
			score := r.RelevanceScore
			if score < float32(threshold) {
				continue
			}
			content := ""
			if r.Fact != nil {
				content = r.Fact.Content
			}
			shown++
			fmt.Fprintf(&b, "%d. (sim: %.3f) %s\n", shown, score, content)
		}
		if shown == 0 {
			msg := fmt.Sprintf("No memories above threshold %.2f for %q.", threshold, args.Query)
			return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
		}
		out := strings.TrimRight(b.String(), "\n")
		return skills.ToolResult{Content: out, Tokens: len(out) / 4}, nil
	}

	// Fallback: local pgvector store, used when the engine is
	// unavailable. Same row set as the engine path since the
	// engine migration.
	if s.store == nil {
		return skills.ToolResult{Error: "memory: store unavailable"}, nil
	}
	searchCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	storeResults, storeErr := s.store.Search(searchCtx, vec, limit, threshold, userID)
	cancel2()
	if storeErr != nil {
		return skills.ToolResult{}, fmt.Errorf("search memory: %w", storeErr)
	}
	if len(storeResults) == 0 {
		msg := fmt.Sprintf("No memories above threshold %.2f for %q.", threshold, args.Query)
		return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Memory search results for %q:\n", args.Query)
	for i, r := range storeResults {
		fmt.Fprintf(&b, "%d. (sim: %.3f) %s\n", i+1, r.Similarity, r.Content)
	}
	content := strings.TrimRight(b.String(), "\n")
	data, _ := json.Marshal(storeResults)
	return skills.ToolResult{Content: content, Data: data, Tokens: len(content) / 4}, nil
}

// --- list_my_memories -------------------------------------------------------

type listMyMemoriesArgs struct {
	Limit  *int   `json:"limit,omitempty"`
	Offset *int   `json:"offset,omitempty"`
	Query  string `json:"query,omitempty"`
}

func (s *Skill) execListMyMemories(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	if s.manager == nil {
		return skills.ToolResult{Error: "memory: manager unavailable — list_my_memories is not configured"}, nil
	}

	var args listMyMemoriesArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}

	limit := 10
	if args.Limit != nil && *args.Limit > 0 {
		limit = *args.Limit
	}
	if limit > 50 {
		limit = 50
	}
	offset := 0
	if args.Offset != nil && *args.Offset >= 0 {
		offset = *args.Offset
	}

	sc, _ := skills.ContextFrom(ctx)
	userID := sc.UserID
	if userID == "" {
		// OWNER-MIGRATION: refuse to attribute memory writes/reads to
		// the legacy "owner" canonical_id when the skill context is
		// missing a user. A blank UserID at this layer means the
		// caller never resolved identity (pipeline.resolveIdentity hit
		// the unmapped path) — the memory tools must refuse rather
		// than silently scope everything to the bootstrap admin.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}

	// When a query is present and we have an embedder + engine, run
	// a semantic search through the engine first and merge its hits
	// with the substring-matched pgvector rows, deduplicated by fact
	// id. Since the engine migration the engine queries the same
	// pgvector table the manager does, so the two paths overlap; the
	// engine path still wins when present because its similarity
	// score is more useful than the manager's substring match.
	//
	// Empty-query case: listing every fact is intentionally not
	// supported as an LLM-facing tool. An unbounded dump is either
	// useless (model wastes tokens on noise) or dangerous (leaks
	// facts across user/agent boundaries).
	type listEntry struct {
		ID        string
		Content   string
		Age       time.Duration
		Score     float32 // non-zero for engine semantic hits; pgvector list has no score
		FromStore bool    // true for rows from pgvector ListMemories (for Data serialization)
		StoreRow  *mem.MemoryRow
	}
	entries := make([]listEntry, 0, limit*2)
	seenID := make(map[string]int, limit*2) // id → index in entries

	if strings.TrimSpace(args.Query) != "" && s.embed != nil && s.engine != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		vec, embedErr := s.embed(embedCtx, args.Query)
		cancel()
		if embedErr == nil {
			engineResults, engineErr := s.engineSemanticSearch(ctx, vec, limit, userID)
			if engineErr == nil {
				for _, r := range engineResults {
					if r.Fact == nil || r.Fact.Id == "" {
						continue
					}
					age := time.Duration(0)
					if r.Fact.CreatedAt != nil {
						age = time.Since(r.Fact.CreatedAt.AsTime()).Truncate(time.Minute)
					}
					entries = append(entries, listEntry{
						ID:      r.Fact.Id,
						Content: r.Fact.Content,
						Age:     age,
						Score:   r.RelevanceScore,
					})
					seenID[r.Fact.Id] = len(entries) - 1
				}
			}
		}
	}

	f := mem.MemoryFilter{
		Substring:        args.Query,
		UserIDFilterMode: mem.UserIDFilterExact,
		UserID:           userID,
	}

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	rows, err := s.manager.ListMemories(listCtx, f, limit, offset)
	cancel()
	if err != nil && len(entries) == 0 {
		return skills.ToolResult{}, fmt.Errorf("list memories: %w", err)
	}

	for i := range rows {
		r := &rows[i]
		if _, dup := seenID[r.ID]; dup {
			// Prefer pgvector row details (scope, created_at) when
			// both paths report the same fact.
			idx := seenID[r.ID]
			entries[idx].StoreRow = r
			entries[idx].FromStore = true
			continue
		}
		entries = append(entries, listEntry{
			ID:        r.ID,
			Content:   r.Content,
			Age:       time.Since(r.CreatedAt).Truncate(time.Minute),
			FromStore: true,
			StoreRow:  r,
		})
		seenID[r.ID] = len(entries) - 1
	}

	if len(entries) == 0 {
		msg := "No memories found."
		if args.Query != "" {
			msg = fmt.Sprintf("No memories matching %q.", args.Query)
		}
		return skills.ToolResult{Content: msg, Tokens: len(msg) / 4}, nil
	}

	// Cap display to requested limit. Entries list may exceed it because
	// engine + pgvector each returned up to `limit` rows pre-merge.
	if len(entries) > limit {
		entries = entries[:limit]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Your memories (showing %d, offset %d):\n", len(entries), offset)
	for i, e := range entries {
		fmt.Fprintf(&b, "%d. %s (id=%s, age=%s)\n", i+1+offset, e.Content, e.ID, e.Age)
	}
	content := strings.TrimRight(b.String(), "\n")

	// Serialize just the pgvector rows into Data — existing callers expect
	// MemoryRow shape there. Engine-only RAM entries don't round-trip today;
	// that's an acceptable limit until Phase B exposes a richer RAM type.
	storeRows := make([]mem.MemoryRow, 0, len(entries))
	for _, e := range entries {
		if e.StoreRow != nil {
			storeRows = append(storeRows, *e.StoreRow)
		}
	}
	data, _ := json.Marshal(storeRows)
	return skills.ToolResult{Content: content, Data: data, Tokens: len(content) / 4}, nil
}

// --- forget_fact ------------------------------------------------------------

type forgetFactArgs struct {
	Query string `json:"query,omitempty"`
	ID    string `json:"id,omitempty"`
}

func (s *Skill) execForgetFact(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	// The engine can delete from RAM on its own, so manager being nil is
	// no longer a hard failure — it just means persistent-tier deletes
	// are unreachable. Fall through and let the engine path handle it.
	if s.manager == nil && s.engine == nil {
		return skills.ToolResult{Error: "memory: neither engine nor manager available — forget_fact is not configured"}, nil
	}

	var args forgetFactArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}

	sc, _ := skills.ContextFrom(ctx)
	userID := sc.UserID
	if userID == "" {
		// OWNER-MIGRATION: refuse to attribute memory writes/reads to
		// the legacy "owner" canonical_id when the skill context is
		// missing a user. A blank UserID at this layer means the
		// caller never resolved identity (pipeline.resolveIdentity hit
		// the unmapped path) — the memory tools must refuse rather
		// than silently scope everything to the bootstrap admin.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}

	targetID := strings.TrimSpace(args.ID)
	targetContent := ""

	// If no explicit ID, search by semantic similarity to find the
	// best match.
	if targetID == "" {
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return skills.ToolResult{Error: "provide either query or id to identify the memory to forget"}, nil
		}
		if s.embed == nil {
			return skills.ToolResult{Error: "memory: embedder unavailable — provide an explicit id instead"}, nil
		}

		embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		vec, err := s.embed(embedCtx, query)
		cancel()
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("embed query: %w", err)
		}

		engineResults, engineErr := s.engineSemanticSearch(ctx, vec, 1, userID)
		if engineErr == nil && len(engineResults) > 0 && engineResults[0].Fact != nil {
			targetID = engineResults[0].Fact.Id
			targetContent = engineResults[0].Fact.Content
		} else if s.store != nil {
			searchCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
			storeResults, storeErr := s.store.Search(searchCtx, vec, 1, 0.5, userID)
			cancel2()
			if storeErr != nil {
				return skills.ToolResult{}, fmt.Errorf("search memory: %w", storeErr)
			}
			if len(storeResults) > 0 {
				targetID = storeResults[0].ID
				targetContent = storeResults[0].Content
			}
		}
		if targetID == "" {
			return skills.ToolResult{Content: fmt.Sprintf("No memory found matching %q.", query)}, nil
		}
	}

	// One tier now: engine.DeleteFact and manager.DeleteMemory both
	// run a DELETE against the same memories row. We call the
	// manager when present (it returns a clean ErrNotFound we can
	// branch on) and fall back to the engine otherwise. The old
	// two-tier delete that called both is gone with the RAM tier.
	deleted := false
	if s.manager != nil {
		// Owner-scoped: an explicit `id` from the model (or a shard
		// turn's tool call) can name any UUID, so the delete predicate
		// itself must enforce that the row belongs to the calling user.
		delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ok, err := s.manager.DeleteMemoryOwned(delCtx, targetID, userID)
		cancel()
		if err != nil {
			return skills.ToolResult{}, fmt.Errorf("delete memory: %w", err)
		}
		deleted = ok
	} else if s.engine != nil {
		vis := &pb.VisibilityContext{UserId: userID}
		delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := s.engine.DeleteFact(delCtx, "", targetID, vis)
		cancel()
		if err == nil && resp != nil && resp.Deleted {
			deleted = true
		}
	}

	if !deleted {
		return skills.ToolResult{Content: fmt.Sprintf("Memory %s not found.", targetID)}, nil
	}

	var content string
	if targetContent != "" {
		content = fmt.Sprintf("Forgot %s: %q", targetID, targetContent)
	} else {
		content = fmt.Sprintf("Forgot %s.", targetID)
	}
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}

// --- correct_fact -----------------------------------------------------------

type correctFactArgs struct {
	Query      string `json:"query"`
	NewContent string `json:"new_content"`
}

func (s *Skill) execCorrectFact(ctx context.Context, params json.RawMessage) (skills.ToolResult, error) {
	if s.manager == nil && s.engine == nil {
		return skills.ToolResult{Error: "memory: neither engine nor manager available — correct_fact is not configured"}, nil
	}

	var args correctFactArgs
	if len(params) > 0 {
		if err := json.Unmarshal(params, &args); err != nil {
			return skills.ToolResult{Error: "invalid params: " + err.Error()}, nil
		}
	}
	query := strings.TrimSpace(args.Query)
	newContent := strings.TrimSpace(args.NewContent)
	if query == "" {
		return skills.ToolResult{Error: "query is required to identify the memory to correct"}, nil
	}
	if newContent == "" {
		return skills.ToolResult{Error: "new_content is required"}, nil
	}

	if s.embed == nil {
		return skills.ToolResult{Error: "memory: embedder unavailable — cannot locate the memory to correct"}, nil
	}

	sc, _ := skills.ContextFrom(ctx)
	userID := sc.UserID
	if userID == "" {
		// OWNER-MIGRATION: refuse to attribute memory writes/reads to
		// the legacy "owner" canonical_id when the skill context is
		// missing a user. A blank UserID at this layer means the
		// caller never resolved identity (pipeline.resolveIdentity hit
		// the unmapped path) — the memory tools must refuse rather
		// than silently scope everything to the bootstrap admin.
		return skills.ToolResult{Error: "memory: no user_id in skill context — caller is unauthenticated"}, nil
	}

	embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	vec, err := s.embed(embedCtx, query)
	cancel()
	if err != nil {
		return skills.ToolResult{}, fmt.Errorf("embed query: %w", err)
	}

	// Locate the target fact by semantic similarity. Engine path
	// preferred (returns the same pgvector rows but with a relevance
	// score); manager.Search is the fallback.
	var targetID, targetContent string
	engineResults, engineErr := s.engineSemanticSearch(ctx, vec, 1, userID)
	if engineErr == nil && len(engineResults) > 0 && engineResults[0].Fact != nil {
		targetID = engineResults[0].Fact.Id
		targetContent = engineResults[0].Fact.Content
	} else if s.store != nil {
		searchCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		storeResults, storeErr := s.store.Search(searchCtx, vec, 1, 0.5, userID)
		cancel2()
		if storeErr != nil {
			return skills.ToolResult{}, fmt.Errorf("search memory: %w", storeErr)
		}
		if len(storeResults) > 0 {
			targetID = storeResults[0].ID
			targetContent = storeResults[0].Content
		}
	}
	if targetID == "" {
		return skills.ToolResult{Content: fmt.Sprintf("No memory found matching %q.", query)}, nil
	}

	// Re-embed the corrected content so future semantic searches
	// surface it by its new meaning rather than the pre-correction
	// vector. Non-fatal: if re-embedding fails, leave the existing
	// embedding in place and let the text update proceed anyway.
	newEmbedCtx, cancelE := context.WithTimeout(ctx, 10*time.Second)
	newVec, newVecErr := s.embed(newEmbedCtx, newContent)
	cancelE()
	if newVecErr != nil {
		newVec = nil
	}

	// One tier now: engine.UpdateFact and manager.UpdateMemoryContent
	// both target the same memories row. Prefer the engine when
	// present (it also updates the embedding column atomically with
	// the content); fall back to the manager otherwise.
	updated := false
	if s.engine != nil {
		vis := &pb.VisibilityContext{UserId: userID}
		updCtx, cancelU := context.WithTimeout(ctx, 5*time.Second)
		resp, err := s.engine.UpdateFact(updCtx, "", targetID, newContent, newVec, vis)
		cancelU()
		if err == nil && resp != nil && resp.Updated {
			updated = true
		}
	}
	if !updated && s.manager != nil {
		changedBy := "user:" + userID
		updCtx, cancelU := context.WithTimeout(ctx, 5*time.Second)
		err := s.manager.UpdateMemoryContent(updCtx, targetID, newContent, changedBy, newVec)
		cancelU()
		if err == nil {
			updated = true
		} else if !strings.Contains(err.Error(), "not found") {
			return skills.ToolResult{}, fmt.Errorf("update memory: %w", err)
		}
	}

	if !updated {
		return skills.ToolResult{Content: fmt.Sprintf("Memory %s not found.", targetID)}, nil
	}

	content := fmt.Sprintf("Corrected %s:\n  was: %s\n  now: %s", targetID, targetContent, newContent)
	return skills.ToolResult{Content: content, Tokens: len(content) / 4}, nil
}
