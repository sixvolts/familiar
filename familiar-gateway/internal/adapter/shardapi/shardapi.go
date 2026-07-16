// Package shardapi implements the sharded external-invocation HTTP
// surface defined in FAMILIAR-SHARDS-PHASE1-SPEC and renamed in
// FAMILIAR-CONSOLE-SPEC Phase A (was internal/adapter/puppet/).
//
//	POST /v1/shards/{id}/invoke
//
// Each request authenticates via X-Familiar-User-Email + a bearer
// token scoped to exactly one shard. The handler resolves the
// (user, shard, token) triple, translates the shard row into a
// pipeline.ShardOverrides envelope, and dispatches through
// pipeline.HandleShard / HandleShardStream. Unlike the trusted OpenAI
// adapter, shard-API requests:
//
//   - ignore model / tools / temperature / max_tokens / user in the
//     body — the shard is the capability envelope, the caller cannot
//     expand it;
//   - never run the router, the Familiar tiered prompt store, the
//     preamble path, or pre-execution tool orchestration;
//   - skip memory retrieval entirely (shards see the shard prompt +
//     prior session turns, nothing else);
//   - tag all downstream writes (pipeline commit, extracted facts,
//     memory-skill saves, session-store rolling summary) with the
//     shard's scope_tag so retrieval isolation works.
//
// Package name is `shardapi` (not `shards`) to avoid shadowing
// internal/shards which owns the persistence layer; the adapter
// reads as "the shard HTTP API."
//
// Observability uses the `[shards]` log prefix so shard-API traffic
// is greppable separately from trusted-path traffic.
package shardapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
	"github.com/google/uuid"
)

// UserLookup is the minimal slice of *identity.Resolver the handler
// needs — a way to go from email → canonical user. Kept as an
// interface so tests can wire a fake without spinning up a DB-backed
// resolver.
type UserLookup interface {
	GetByEmail(ctx context.Context, email string) (*identity.User, error)
}

// Pipeline is the subset of *pipeline.Pipeline the handler uses.
// Declared here (rather than imported concretely) so tests can stub
// the dispatch surface with a fake that doesn't need a full pipeline.
// Production always wires in the real pipeline.
type Pipeline interface {
	HandleShard(ctx context.Context, sess *session.Session, userMsg string, overrides *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error)
	HandleShardStream(
		ctx context.Context,
		sess *session.Session,
		userMsg string,
		overrides *pipeline.ShardOverrides,
		onChunk func(string),
		onReasoningChunk func(string),
		onStatus func(string),
	) (string, *pipeline.RouteInfo, error)
}

// Handler serves the sharded HTTP API behind a single ServeHTTP
// entry point. Registered on the openai adapter's mux at the /v1/
// prefix.
type Handler struct {
	store    shards.Store
	pipe     Pipeline
	sessions *session.Manager
	users    UserLookup

	mux *http.ServeMux
}

// New constructs a Handler. All four dependencies are required;
// passing nil for any is a wiring bug and will panic on the first
// invocation rather than silently no-op.
func New(store shards.Store, pipe Pipeline, sm *session.Manager, users UserLookup) *Handler {
	h := &Handler{
		store:    store,
		pipe:     pipe,
		sessions: sm,
		users:    users,
	}
	h.mux = http.NewServeMux()
	// Go 1.22+ method-and-wildcard routing. The {id} wildcard is
	// retrieved via r.PathValue("id"). Only POST is defined — any
	// other verb on the same path yields a 405 from ServeMux.
	h.mux.HandleFunc("POST /v1/shards/{id}/invoke", h.handleInvoke)
	return h
}

// ServeHTTP delegates to the internal mux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// -----------------------------------------------------------------------------
// Request / response types
// -----------------------------------------------------------------------------

// invokeRequest is the accepted body shape. Fields commented "ignored"
// are parsed only so the JSON decoder doesn't balk — the handler
// honors the shard's configured values, never the caller's. This is
// deliberate per the spec: the shard IS the capability envelope, and
// the caller cannot expand it by putting a different model/temperature/
// tools list in their body.
type invokeRequest struct {
	Messages  []invokeMessage `json:"messages"`
	SessionID string          `json:"session_id,omitempty"`
	Stream    *bool           `json:"stream,omitempty"`

	// Accepted but ignored — see spec §"Body fields not accepted".
	Model       string          `json:"model,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	User        string          `json:"user,omitempty"`
}

type invokeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type invokeResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []choice      `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
	Shard   shardEnvelope `json:"shard"`
}

type choice struct {
	Index        int            `json:"index"`
	Message      *invokeMessage `json:"message,omitempty"`
	Delta        *invokeMessage `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// shardEnvelope is the Familiar-specific response field. Callers use
// it to log which shard produced which output and, for persistent
// shards, to preserve the session_id across turns.
type shardEnvelope struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// -----------------------------------------------------------------------------
// Auth
// -----------------------------------------------------------------------------

// authResult is what a successful authenticate call yields. The
// handler uses this to build the session + ShardOverrides.
type authResult struct {
	canonicalID string
	shard       *shards.Shard
	token       *shards.Token
}

// authenticate is the single place status codes are decided. The
// caller gets back either an authResult or a (statusCode, message)
// pair it should surface verbatim to the client. Logs carry the
// token prefix (never plaintext) so failed attempts are auditable.
func (h *Handler) authenticate(ctx context.Context, r *http.Request, shardID string) (*authResult, int, string) {
	email := strings.TrimSpace(r.Header.Get("X-Familiar-User-Email"))
	if email == "" {
		return nil, http.StatusUnauthorized, "missing X-Familiar-User-Email header"
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, http.StatusUnauthorized, "missing or malformed Authorization: Bearer <token>"
	}
	plaintext := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if plaintext == "" {
		return nil, http.StatusUnauthorized, "missing bearer token"
	}

	// Redacted form used in all subsequent log lines so failed attempts
	// don't leak the plaintext.
	tokenPrefix := plaintext
	if len(tokenPrefix) > 14 {
		tokenPrefix = tokenPrefix[:14] + "..."
	}

	tok, err := h.store.ValidateToken(ctx, plaintext)
	switch {
	case errors.Is(err, shards.ErrTokenRevoked):
		log.Printf("[shards] auth failed: revoked token email=%s shard=%s token=%s",
			email, shardID, tokenPrefix)
		return nil, http.StatusForbidden, "token revoked"
	case errors.Is(err, shards.ErrTokenNotFound):
		log.Printf("[shards] auth failed: unknown token email=%s shard=%s token=%s",
			email, shardID, tokenPrefix)
		return nil, http.StatusUnauthorized, "invalid token"
	case err != nil:
		log.Printf("[shards] auth error: token validation failed: %v", err)
		return nil, http.StatusInternalServerError, "token validation failed"
	}

	if tok.ShardID != shardID {
		log.Printf("[shards] auth failed: token/shard mismatch email=%s url_shard=%s token_shard=%s token=%s",
			email, shardID, tok.ShardID, tok.TokenPrefix)
		return nil, http.StatusForbidden, "token is not valid for this shard"
	}

	user, err := h.users.GetByEmail(ctx, email)
	if err != nil {
		log.Printf("[shards] auth error: user lookup failed: %v", err)
		return nil, http.StatusInternalServerError, "user lookup failed"
	}
	if user == nil {
		log.Printf("[shards] auth failed: unknown email email=%s shard=%s token=%s",
			email, shardID, tok.TokenPrefix)
		return nil, http.StatusForbidden, "email is not registered"
	}
	if user.ID != tok.OwnerID {
		log.Printf("[shards] auth failed: email/owner mismatch email=%s user=%s token_owner=%s shard=%s token=%s",
			email, user.ID, tok.OwnerID, shardID, tok.TokenPrefix)
		return nil, http.StatusForbidden, "email does not match token owner"
	}
	if user.Status != identity.StatusApproved {
		log.Printf("[shards] auth failed: user not approved email=%s status=%s",
			email, user.Status)
		return nil, http.StatusForbidden, "user is not approved: " + string(user.Status)
	}

	sh, err := h.store.GetShard(ctx, shardID)
	switch {
	case errors.Is(err, shards.ErrShardNotFound):
		return nil, http.StatusNotFound, "shard not found"
	case err != nil:
		log.Printf("[shards] shard lookup error: %v", err)
		return nil, http.StatusInternalServerError, "shard lookup failed"
	}
	if !sh.Active() {
		return nil, http.StatusGone, "shard is disabled"
	}
	if sh.OwnerID != user.ID {
		// Defensive: token ownership already matched, but double-check.
		log.Printf("[shards] auth failed: shard owner drift email=%s shard_owner=%s",
			email, sh.OwnerID)
		return nil, http.StatusForbidden, "shard owner mismatch"
	}

	return &authResult{
		canonicalID: user.ID,
		shard:       sh,
		token:       tok,
	}, http.StatusOK, ""
}

// -----------------------------------------------------------------------------
// Invoke
// -----------------------------------------------------------------------------

// handleInvoke is the single route. Flow:
//  1. Parse body early so a malformed request doesn't consume auth time.
//  2. Authenticate — resolves (user, shard, token) or short-circuits with
//     the appropriate status code.
//  3. Build ShardOverrides from the shard row.
//  4. Build/lookup session keyed on shard+session_id+email so top-level
//     and shard-invoked traffic never collide.
//  5. Dispatch streaming or non-streaming path through the pipeline.
//  6. TouchToken (best-effort) so last_used_at stays truthful.
func (h *Handler) handleInvoke(w http.ResponseWriter, r *http.Request) {
	shardID := r.PathValue("id")
	if shardID == "" {
		writeError(w, http.StatusNotFound, "shard id missing in path")
		return
	}

	// Bound the body so a misbehaving caller can't pin memory.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req invokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid request body: "+err.Error())
		return
	}
	userMsg, userMsgOK := lastUserMessage(req.Messages)
	if !userMsgOK {
		writeError(w, http.StatusUnprocessableEntity, "no user message found in messages[]")
		return
	}

	ctx := r.Context()
	auth, status, msg := h.authenticate(ctx, r, shardID)
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}

	// One-line audit record of a successful auth before we hand off to
	// the pipeline. Model is shard-configured so it's visible up front.
	tierLabel := auth.shard.TierPreference
	if tierLabel == "" {
		tierLabel = auth.shard.ModelPreference
	}
	if tierLabel == "" {
		tierLabel = "(router)"
	}
	log.Printf("[shards] invoke shard=%s user=%s token=%s tier=%s",
		auth.shard.ID,
		trimEmailForLog(r.Header.Get("X-Familiar-User-Email")),
		auth.token.TokenPrefix,
		tierLabel,
	)

	// Touch the token asynchronously — a slow or failed update must not
	// block the invocation. Fire-and-forget with a detached context so
	// the request's cancellation doesn't cascade.
	tokenID := auth.token.ID
	store := h.store
	go func() {
		touchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.TouchToken(touchCtx, tokenID); err != nil {
			log.Printf("[shards] token touch failed (non-fatal): id=%s err=%v", tokenID, err)
		}
	}()

	overrides := buildOverrides(auth.shard)
	sessionID, sess := h.sessionFor(auth.shard, auth.canonicalID, req.SessionID, r.Header.Get("X-Familiar-User-Email"))

	streaming := false
	if req.Stream != nil {
		streaming = *req.Stream
	}

	start := time.Now()
	completionID := "chatcmpl-" + uuid.NewString()[:8]
	model := modelDisplay(auth.shard)

	if streaming {
		h.handleStreaming(ctx, w, sess, userMsg, overrides, completionID, model, auth.shard.ID, sessionID, start)
	} else {
		h.handleNonStreaming(ctx, w, sess, userMsg, overrides, completionID, model, auth.shard.ID, sessionID, start)
	}
}

// sessionFor builds a session appropriate to the shard's persistence
// mode. Persistent shards get a manager-registered session keyed on
// (shard, session_id, email) so concurrent turns within the same
// session_id share state and hit the persisted summary.
//
// Ephemeral shards get a fresh in-memory session that is NOT registered
// with the manager — each invocation is fully independent. The pipeline
// respects this via SkipSessionHydration + SkipCommit on the overrides,
// so the unregistered session never gets a persistence round-trip.
//
// Returns the caller-visible session_id and the Session object.
func (h *Handler) sessionFor(shard *shards.Shard, canonicalID, reqSessionID, email string) (string, *session.Session) {
	if shard.Persistence == shards.PersistenceEphemeral {
		// Fresh session every time, never registered so no lookup collisions.
		now := time.Now()
		sess := &session.Session{
			ID:         uuid.NewString(),
			ChannelID:  "shards:" + shard.ID,
			SenderID:   email,
			CreatedAt:  now,
			LastActive: now,
			Metadata:   map[string]string{},
		}
		sess.SetIdentity("shards", canonicalID)
		// Ephemeral invocations don't expose a session_id — there's
		// nothing for the caller to resume.
		return "", sess
	}

	sessionID := reqSessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// Include session_id in the channel so two different sessions for
	// the same (shard, caller) are distinct objects in the manager.
	channelID := "shards:" + shard.ID + ":" + sessionID
	sess := h.sessions.GetOrCreateWithID(sessionID, channelID, email)
	sess.SetIdentity("shards", canonicalID)
	return sessionID, sess
}

// buildOverrides translates a stored Shard row into the pipeline
// envelope. The actual translation lives in
// pipeline.OverridesForShard — shared with the scheduled-actions
// runner so the two shard entry points can't drift
// (SCHEDULED-ACTIONS-SPEC).
func buildOverrides(sh *shards.Shard) *pipeline.ShardOverrides {
	return pipeline.OverridesForShard(sh)
}

// modelDisplay picks a stable model identifier for the response
// envelope. Shards with an explicit model preference use that; others
// advertise the shard ID (routing may pick any tier, so there's no
// single model to claim).
func modelDisplay(sh *shards.Shard) string {
	if sh.ModelPreference != "" {
		return sh.ModelPreference
	}
	return "shard:" + sh.ID
}

// -----------------------------------------------------------------------------
// Non-streaming response
// -----------------------------------------------------------------------------

func (h *Handler) handleNonStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	sess *session.Session,
	userMsg string,
	overrides *pipeline.ShardOverrides,
	completionID, model, shardID, sessionID string,
	start time.Time,
) {
	text, info, err := h.pipe.HandleShard(ctx, sess, userMsg, overrides)
	if err != nil {
		log.Printf("[shards] invoke failed: shard=%s err=%v", shardID, err)
		writeError(w, http.StatusInternalServerError, "invocation failed")
		return
	}

	finish := "stop"
	resp := invokeResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []choice{{
			Index: 0,
			Message: &invokeMessage{
				Role:    "assistant",
				Content: text,
			},
			FinishReason: &finish,
		}},
		Shard: shardEnvelope{
			ID:        shardID,
			SessionID: sessionID,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	log.Printf("[shards] invoke ok shard=%s model=%s latency=%s mem_hits=%d",
		shardID, usedModel(info, model), time.Since(start).Round(time.Millisecond), memHits(info))
}

// -----------------------------------------------------------------------------
// Streaming response (SSE)
// -----------------------------------------------------------------------------

func (h *Handler) handleStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	sess *session.Session,
	userMsg string,
	overrides *pipeline.ShardOverrides,
	completionID, model, shardID, sessionID string,
	start time.Time,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported by response writer")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(c choice) {
		mu.Lock()
		defer mu.Unlock()
		chunk := invokeResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []choice{c},
			Shard: shardEnvelope{
				ID:        shardID,
				SessionID: sessionID,
			},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			log.Printf("[shards] sse marshal error: %v", err)
			return
		}
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(b)
		_, _ = io.WriteString(w, "\n\n")
		flusher.Flush()
	}

	// Initial role chunk.
	send(choice{
		Index: 0,
		Delta: &invokeMessage{Role: "assistant"},
	})

	onChunk := func(s string) {
		send(choice{
			Index: 0,
			Delta: &invokeMessage{Content: s},
		})
	}
	// Reasoning and status chunks are discarded for the shard surface
	// — unlike the trusted OpenAI adapter, shards don't surface
	// Familiar's reasoning UX to callers. Extraction shards just want
	// the final JSON; routing metadata is out of band (logs).
	_, info, err := h.pipe.HandleShardStream(ctx, sess, userMsg, overrides, onChunk, nil, nil)
	if err != nil {
		log.Printf("[shards] invoke failed: shard=%s err=%v", shardID, err)
		send(choice{
			Index: 0,
			Delta: &invokeMessage{Content: fmt.Sprintf("\n\n[error: %v]", err)},
		})
	}

	finish := "stop"
	send(choice{
		Index:        0,
		Delta:        &invokeMessage{},
		FinishReason: &finish,
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	log.Printf("[shards] invoke ok shard=%s model=%s latency=%s mem_hits=%d (streamed)",
		shardID, usedModel(info, model), time.Since(start).Round(time.Millisecond), memHits(info))
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// lastUserMessage scans messages[] back-to-front for the last
// role=user entry. Spec says the caller may include their own system
// messages; those are currently discarded by the shard path (the
// shard's system prompt is the only non-user context the LLM sees).
// Returning false when no user role is present is how the handler
// produces the 422 the spec requires.
func lastUserMessage(msgs []invokeMessage) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content != "" {
			return msgs[i].Content, true
		}
	}
	return "", false
}

// writeError is the canonical error writer. All errors are JSON
// bodies of shape {"error": "..."} per the spec, regardless of status
// code.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

// trimEmailForLog keeps log lines bounded in width when a pathological
// email shows up. The full email is still available via the admin UI
// or audit logs; the per-invocation log is a quick-scan summary.
func trimEmailForLog(email string) string {
	if len(email) > 64 {
		return email[:64] + "..."
	}
	return email
}

func usedModel(info *pipeline.RouteInfo, fallback string) string {
	if info == nil || info.ModelID == "" {
		return fallback
	}
	return info.ModelID
}

func memHits(info *pipeline.RouteInfo) int {
	if info == nil {
		return 0
	}
	return info.MemHits
}
