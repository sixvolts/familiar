// Package native is the gateway's HTTP adapter speaking the
// Familiar-native chat protocol. CHAT-REARCH §"Phase 0 (HTTP entry)"
// retired the OpenAI-shape entry — clients no longer send a messages[]
// array; the gateway is the single source of truth for conversation
// history.
//
// Wire shape:
//
//	POST /api/chat
//	  body:    {"message": string, "conversation_id"?: string, "channel_id"?: string, "thinking"?: "auto"|"on"|"off"}
//	  auth:    admin session cookie (the only accepted identity source)
//	  headers: Accept (text/event-stream for streaming)
//	  reply:   text/event-stream when accept includes event-stream, JSON otherwise
//	  events:  token | reasoning | status | done | error
//
//	GET  /api/health → {"status":"ok"}
//
//	GET  /events/{session_id} → memlog SSE stream (CHAT-REARCH S5)
//
// The adapter also hosts the same console/admin and shards mounts the
// previous adapter did, so the gateway still exposes one HTTP listener
// that fronts every surface.
package native

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
	"github.com/familiar/gateway/internal/memevents"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
)

// ChatRequest is the native /api/chat request body. No messages[],
// no model field — gateway holds the conversation, classifier picks
// the model. CHAT-REARCH §"Phase 0".
type ChatRequest struct {
	Message   string `json:"message"`
	ChannelID string `json:"channel_id,omitempty"`
	// ConversationID is the persistent conversation UUID the
	// workspace assigns. When present it doubles as the in-memory
	// session id so a gateway restart can rehydrate the session's
	// verbatim turns from the messages table without losing the
	// thread. Optional — adapters without a persisted conversation
	// (CLI, bare HTTP) leave it empty and get the legacy one-
	// session-per-(channel, sender) behavior.
	// See SESSION-HYDRATION.md.
	ConversationID string `json:"conversation_id,omitempty"`
	// Thinking is an optional per-request override. "auto" (default)
	// lets the classifier decide; "on" forces high-effort thinking;
	// "off" disables thinking entirely. Reserved — the override
	// plumbing lands when the developer-mode toggle ships; today
	// auto/empty/unknown all collapse to "let the classifier
	// decide".
	Thinking string `json:"thinking,omitempty"`
}

// ChatResponse is the non-streaming /api/chat response. Streaming
// callers receive an SSE event stream instead and never see this
// shape; clients pick by Accept header.
type ChatResponse struct {
	SessionID string `json:"session_id"`
	ModelID   string `json:"model_id,omitempty"`
	Content   string `json:"content"`
	MemHits   int    `json:"mem_hits,omitempty"`
}

// SessionReader resolves an HTTP request's admin session cookie to a
// canonical user ID. Implemented by *admin.Handler.
type SessionReader interface {
	UserIDFromRequest(r *http.Request) (string, bool)
}

// ConversationOwner verifies that a client-supplied conversation_id
// belongs to the authenticated caller before the adapter binds it to
// an in-memory session. Implemented by *admin.ConversationStore.
// When unset (no DB / dev), the adapter trusts the supplied id —
// acceptable because without the store the pipeline also has no
// conversation read/write path, so the IDOR has no teeth.
type ConversationOwner interface {
	OwnsConversation(ctx context.Context, conversationID, userID string) (bool, error)
}

// ShardChatTarget is the resolved binding for a shard-backed
// conversation (SKILL-PACKAGES-SPEC Phase 1: chat with a shard). A
// nil target means the conversation runs the trusted path.
type ShardChatTarget struct {
	ShardID string
	// Ephemeral mirrors the shard's persistence mode: ephemeral
	// shards get a fresh, unregistered session per message (no
	// cross-turn pipeline memory), matching /v1 invoke semantics.
	// The conversation transcript still persists for display.
	Ephemeral bool
	Overrides *pipeline.ShardOverrides
}

// ShardChatResolver maps an owned conversation to its shard binding.
// Wired from main.go so the adapter stays free of store imports.
// Contract: (nil, "", nil) = trusted-path conversation; a non-empty
// refusal is a policy rejection surfaced to the caller verbatim
// (shard disabled / chat disabled / not yours); err is an internal
// failure. The adapter checks conversation OWNERSHIP before calling.
type ShardChatResolver func(ctx context.Context, conversationID, userID string) (t *ShardChatTarget, refusal string, err error)

// Adapter is the HTTP frontend.
type Adapter struct {
	pipeline      *pipeline.Pipeline
	sessions      *session.Manager
	engine        engine.Service
	cfg           config.HTTPConfig
	verbose       bool
	adminHandler  http.Handler
	shardsHandler http.Handler
	memEvents     *memevents.Bus
	sessionReader SessionReader
	convOwner     ConversationOwner
	shardChat     ShardChatResolver
	// inflight counts concurrent in-flight /api/chat requests per
	// canonical user id, bounding how much of the single local chat
	// model one user can occupy at once. Entries are deleted at zero
	// so the map doesn't grow with the user base. Guarded by mu.
	inflight map[string]int
	mu       sync.RWMutex
}

const (
	// maxChatBodyBytes caps the decoded request body on the chat
	// endpoints. A user message can carry a paste, but 1 MiB is far
	// more than any real prompt; beyond it is abuse. Matches the
	// shard API's MaxBytesReader.
	maxChatBodyBytes = 1 << 20

	// maxInflightPerUser bounds concurrent /api/chat requests for one
	// canonical user. Generous enough for legitimate multi-tab use,
	// low enough that one external user can't monopolize the model.
	maxInflightPerUser = 4
)

// acquireSlot reserves one of the user's in-flight chat slots,
// returning false when they're already at the cap. Pair every true
// return with a releaseSlot.
func (a *Adapter) acquireSlot(userID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight == nil {
		a.inflight = make(map[string]int)
	}
	if a.inflight[userID] >= maxInflightPerUser {
		return false
	}
	a.inflight[userID]++
	return true
}

// releaseSlot returns a slot acquired by acquireSlot, deleting the
// map entry when the user's count returns to zero.
func (a *Adapter) releaseSlot(userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight[userID] <= 1 {
		delete(a.inflight, userID)
		return
	}
	a.inflight[userID]--
}

// New constructs the adapter. SetAdminHandler / SetShardsHandler /
// SetMemEvents attach optional collaborators after construction so
// the wiring in main.go stays declarative.
func New(p *pipeline.Pipeline, sm *session.Manager, eng engine.Service, cfg config.HTTPConfig, verbose bool) *Adapter {
	return &Adapter{
		pipeline: p,
		sessions: sm,
		engine:   eng,
		cfg:      cfg,
		verbose:  verbose,
	}
}

// SetSessionReader wires the admin session cookie resolver — the sole
// identity source for /api/chat. When unset, every chat request is
// rejected with 401 (there's no other way to identify the caller).
func (a *Adapter) SetSessionReader(sr SessionReader) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionReader = sr
}

// SetConversationOwner wires the conversation-ownership check used to
// gate a client-supplied conversation_id before binding it to a
// session. Optional — nil trusts the supplied id (dev / no-DB).
func (a *Adapter) SetConversationOwner(c ConversationOwner) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.convOwner = c
}

// SetShardChatResolver wires owner-side shard chat (chat with a
// shard through the normal workspace surface). Optional — nil means
// every conversation runs the trusted path, shard-bound rows
// included, so main.go must wire this whenever conversations can
// carry shard bindings (i.e. whenever the conversation store and
// shard store are both attached).
func (a *Adapter) SetShardChatResolver(r ShardChatResolver) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shardChat = r
}

// SetAdminHandler mounts the admin/console handler at both /console/
// and /admin/ (the latter 301s to the former internally; both
// registrations are required so the redirect lands somewhere).
func (a *Adapter) SetAdminHandler(h http.Handler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adminHandler = h
}

// SetShardsHandler mounts the shard-invocation handler at /v1/.
// Kept under /v1/ for now so existing shard tokens / clients keep
// working unchanged through the chat-protocol migration.
func (a *Adapter) SetShardsHandler(h http.Handler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shardsHandler = h
}

// SetMemEvents wires the memlog SSE stream at /events/{session_id}.
// CHAT-REARCH S5.
func (a *Adapter) SetMemEvents(b *memevents.Bus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.memEvents = b
}

func (a *Adapter) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("POST /api/chat/stop", a.handleStop)
	mux.HandleFunc("POST /api/chat/title", a.handleTitle)
	mux.HandleFunc("GET /api/health", a.handleHealth)

	if a.adminHandler != nil {
		mux.Handle("/console/", a.adminHandler)
		mux.Handle("/admin/", a.adminHandler)
		mux.Handle("/p/", a.adminHandler) // public page-share render (host-gated inside handler)
		log.Printf("[http] console mounted at /console/ (legacy /admin/ redirects)")
	}
	if a.shardsHandler != nil {
		mux.Handle("/v1/", a.shardsHandler)
		log.Printf("[http] shards invocation mounted at /v1/shards/")
	}
	if a.memEvents != nil {
		mux.HandleFunc("GET /events/{session_id}", a.serveMemEvents)
		log.Printf("[http] memlog events mounted at /events/{session_id} (session-gated)")
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"service": "familiar-gateway",
				"status":  "ok",
			})
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// Run starts the HTTP server. Blocks until ctx is cancelled or the
// server fails. Graceful shutdown waits up to 5s for in-flight
// requests.
func (a *Adapter) Run(ctx context.Context) error {
	mux := a.buildMux()

	addr := a.cfg.ListenAddr
	if addr == "" {
		addr = "0.0.0.0:8000"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           withCORS(withLogging(mux)),
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      600 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("[http] Listening on %s", addr)
	log.Printf("[http] Native chat at POST /api/chat; memlog at /events/{session_id}")

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}
	return nil
}

// handleChat is the native /api/chat handler. Accept-header content
// negotiation: text/event-stream → SSE, anything else → JSON.
func (a *Adapter) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Identity resolution. The admin session cookie is the ONLY
	// accepted identity source: it carries a server-verified canonical
	// user id, set by SessionStore.Validate during the WebAuthn
	// handshake, and the cookie's status is re-checked on every
	// request inside UserIDFromRequest.
	//
	// The legacy X-User-Email header path was removed (EXTERNAL-
	// READINESS-REVIEW.md): it only proved an email was *approved*,
	// never that the caller owned it, so honoring it on any reachable
	// interface was an impersonation bypass. Senderless requests
	// return 401; authenticate via the workspace session.
	senderID := ""
	if a.sessionReader != nil {
		if uid, ok := a.sessionReader.UserIDFromRequest(r); ok {
			senderID = uid
		}
	}
	if senderID == "" {
		log.Printf("[http] no identity (no valid session cookie) — rejecting")
		writeError(w, http.StatusUnauthorized, "no user identity: authenticate via the workspace session")
		return
	}

	// Per-user concurrency cap on the expensive chat path: one user
	// can't tie up more than maxInflightPerUser slots of the single
	// local model at once. Acquired after identity so the cap is
	// per canonical user, released when the request returns.
	if !a.acquireSlot(senderID) {
		log.Printf("[http] rate-limited %q: %d concurrent requests", senderID, maxInflightPerUser)
		writeError(w, http.StatusTooManyRequests, "too many concurrent requests; wait for one to finish")
		return
	}
	defer a.releaseSlot(senderID)

	channelID := req.ChannelID
	if channelID == "" {
		channelID = "workspace"
	}
	// When the caller supplies a conversation_id (workspace always
	// does), use it as the in-memory session id so post-restart
	// hydration can find the matching messages rows. Without it,
	// fall back to one-session-per-(channel, sender).
	//
	// SECURITY (EXTERNAL-READINESS-REVIEW.md P0): the conversation_id
	// becomes the session key, which drives both turn hydration
	// (reads the conversation's messages) and intermediate-message
	// persistence (writes into it). It is client-supplied, so we
	// MUST confirm the caller owns it first — otherwise a user who
	// knows another user's conversation UUID could read and write
	// that conversation. On any mismatch we reject rather than
	// silently falling back, so a real bug surfaces instead of
	// quietly detaching the user from their thread.
	a.mu.RLock()
	convOwner := a.convOwner
	shardChat := a.shardChat
	a.mu.RUnlock()
	var sess *session.Session
	var shardTarget *ShardChatTarget
	if convID := strings.TrimSpace(req.ConversationID); convID != "" {
		if convOwner != nil {
			owned, err := convOwner.OwnsConversation(r.Context(), convID, senderID)
			if err != nil {
				log.Printf("[http] conversation ownership check error: %v", err)
				writeError(w, http.StatusInternalServerError, "could not verify conversation")
				return
			}
			if !owned {
				log.Printf("[http] rejected conversation_id %q not owned by %q", convID, senderID)
				writeError(w, http.StatusForbidden, "conversation not found")
				return
			}
		}
		// Shard-bound conversation? (SKILL-PACKAGES-SPEC Phase 1.)
		// Resolved on every message — a shard disabled or deleted
		// mid-conversation refuses the NEXT turn, mirroring how the
		// authz middleware re-checks user status per request.
		if shardChat != nil {
			t, refusal, err := shardChat(r.Context(), convID, senderID)
			if err != nil {
				log.Printf("[http] shard chat resolve error for %q: %v", convID, err)
				writeError(w, http.StatusInternalServerError, "could not resolve conversation target")
				return
			}
			if refusal != "" {
				writeError(w, http.StatusConflict, refusal)
				return
			}
			shardTarget = t
		}
		if shardTarget != nil && shardTarget.Ephemeral {
			// Fresh, unregistered session per message — same posture
			// as ephemeral /v1 invokes (shardapi.sessionFor). The
			// overrides carry SkipSessionHydration + SkipCommit so
			// this session never round-trips persistence.
			now := time.Now()
			sess = &session.Session{
				ID:         convID + ":" + fmt.Sprintf("%d", now.UnixNano()),
				ChannelID:  "shards:" + shardTarget.ShardID,
				SenderID:   senderID,
				CreatedAt:  now,
				LastActive: now,
				Metadata:   map[string]string{},
			}
		} else {
			sess = a.sessions.GetOrCreateWithID(convID, channelID, senderID)
		}
	} else {
		sess = a.sessions.GetOrCreate(channelID, senderID)
	}
	// The session cookie's user id is already a verified canonical
	// ID, so set it directly — the pipeline's identity resolver then
	// has nothing to re-resolve.
	sess.SetIdentity("workspace", senderID)

	wantsStream := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	ctx := r.Context()
	if wantsStream {
		a.handleStreaming(ctx, w, sess, req.Message, shardTarget)
	} else {
		a.handleNonStreaming(ctx, w, sess, req.Message, shardTarget)
	}
}

// StopRequest is the POST /api/chat/stop body. session_id is the
// authoritative turn key — the value the gateway emitted in the SSE
// "session" event at the start of the stream, which is exactly what the
// pipeline registered its turn under. conversation_id is accepted as a
// fallback for the desktop path, where the session id equals the
// conversation id.
type StopRequest struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
}

// handleStop cuts the in-flight turn for a session when the user presses
// Stop. Unlike a client disconnect — which detached turns deliberately
// ignore so an abandoned stream still finishes and persists — this is an
// explicit request to end generation now: the pipeline cancels the turn's
// generation context server-side (freeing the model) and commits the
// partial produced so far, keeping persisted history in sync with what
// the user saw.
//
// Fail-closed auth: a valid session cookie is required, and the target
// session's owner must match the caller. The session id is the stop key
// (freely known to the owning client from the SSE stream), so verifying
// the in-memory session's owner is what prevents a cross-user stop.
func (a *Adapter) handleStop(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	var req StopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	key := strings.TrimSpace(req.SessionID)
	if key == "" {
		key = strings.TrimSpace(req.ConversationID)
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	senderID := ""
	if a.sessionReader != nil {
		if uid, ok := a.sessionReader.UserIDFromRequest(r); ok {
			senderID = uid
		}
	}
	if senderID == "" {
		writeError(w, http.StatusUnauthorized, "no user identity: authenticate via the workspace session")
		return
	}

	// Resolve the live session and confirm the caller owns it. A missing
	// session isn't an error — the turn already finished, or the key is
	// stale — it just means there's nothing to stop.
	sess, ok := a.sessions.Get(key)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"stopped": false})
		return
	}
	if sess.UserID() != senderID {
		log.Printf("[http] stop: rejected session %q not owned by %q", key, senderID)
		writeError(w, http.StatusForbidden, "session not found")
		return
	}

	stopped := a.pipeline.StopTurn(key)
	if stopped {
		log.Printf("[http] stop: cut in-flight turn for session %q (user %q)", key, senderID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"stopped": stopped})
}

func (a *Adapter) handleStreaming(ctx context.Context, w http.ResponseWriter, sess *session.Session, userMsg string, shardTarget *ShardChatTarget) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Familiar-Session-ID", sess.ID)
	w.Header().Set("Access-Control-Expose-Headers", "X-Familiar-Session-ID")
	w.WriteHeader(http.StatusOK)

	// writeMu serializes every write to w. The keepalive goroutine below
	// runs concurrently with the pipeline's onChunk/onStatus callbacks,
	// so all SSE frames (events and heartbeat comments) must go out under
	// this lock or they interleave and corrupt the stream.
	var writeMu sync.Mutex

	emit := func(eventName string, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		writeMu.Lock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, body)
		flusher.Flush()
		writeMu.Unlock()
	}

	emit("session", map[string]string{"session_id": sess.ID})

	// Keepalive heartbeat. The model can sit in a silent tool loop (skill
	// loads, research spawn, one long decode) for tens of seconds with no
	// tokens on the wire. The front proxy (tailscale serve) drops a
	// connection it sees as idle, which surfaces in the browser as
	// "Stream error: network error" even though the turn (and any spawned
	// research run) is fine. A bare SSE comment every 15s keeps the
	// connection warm end-to-end; comment lines are ignored by the client
	// parser, so the UI never sees them.
	hbStop := make(chan struct{})
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	go func() {
		defer hbDone.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				writeMu.Lock()
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
				writeMu.Unlock()
			}
		}
	}()
	defer func() {
		close(hbStop)
		hbDone.Wait()
	}()

	onChunk := func(chunk string) { emit("token", map[string]string{"chunk": chunk}) }
	onReasoning := func(reasoning string) { emit("reasoning", map[string]string{"chunk": reasoning}) }
	onStatus := func(status string) { emit("status", map[string]string{"message": status}) }
	var info *pipeline.RouteInfo
	var finalContent string
	var err error
	if shardTarget != nil {
		// Owner-side shard chat: same stream, the shard's envelope.
		finalContent, info, err = a.pipeline.HandleShardStream(ctx, sess, userMsg, shardTarget.Overrides, onChunk, onReasoning, onStatus)
	} else {
		finalContent, info, err = a.pipeline.HandleStream(ctx, sess, userMsg, nil, onChunk, onReasoning, onStatus)
	}
	if err != nil {
		emit("error", map[string]string{"message": err.Error()})
		emit("done", map[string]any{
			"session_id": sess.ID,
			"finish":     "error",
		})
		return
	}

	finish := "stop"
	if info != nil && info.Stopped {
		finish = "stopped"
	}
	donePayload := map[string]any{
		"session_id": sess.ID,
		"finish":     finish,
	}
	// Include the authoritative parsed content so the UI can replace
	// whatever was streamed (which may include leaked reasoning) with
	// the clean final answer.
	if finalContent != "" {
		donePayload["content"] = finalContent
	}
	// Include post-hoc reasoning (from formatters that split untagged
	// chain-of-thought) so the UI can populate the thinking bubble.
	if info != nil && info.ReasoningContent != "" {
		donePayload["reasoning_content"] = info.ReasoningContent
	}
	if shardTarget != nil {
		donePayload["shard_id"] = shardTarget.ShardID
	}
	if info != nil {
		if info.ModelID != "" {
			donePayload["model_id"] = info.ModelID
		}
		donePayload["mem_hits"] = info.MemHits
		if info.InputTokens > 0 {
			donePayload["input_tokens"] = info.InputTokens
		}
		if info.OutputTokens > 0 {
			donePayload["output_tokens"] = info.OutputTokens
		}
		if info.DecodeMs > 0 {
			donePayload["decode_ms"] = info.DecodeMs
		}
		// A research note written this turn (inline quick/standard path)
		// — the workspace auto-opens it in a side pane and links it in
		// chat. Empty PageSlug means the turn wrote no research note.
		if info.ResearchNote.PageSlug != "" {
			donePayload["research_note"] = map[string]string{
				"book_slug": info.ResearchNote.BookSlug,
				"page_slug": info.ResearchNote.PageSlug,
				"title":     info.ResearchNote.Title,
			}
		}
	}
	emit("done", donePayload)

	if a.verbose && info != nil {
		log.Printf("[http] completed: model=%s mem_hits=%d", info.ModelID, info.MemHits)
	}
}

func (a *Adapter) handleNonStreaming(ctx context.Context, w http.ResponseWriter, sess *session.Session, userMsg string, shardTarget *ShardChatTarget) {
	var response string
	var info *pipeline.RouteInfo
	var err error
	if shardTarget != nil {
		response, info, err = a.pipeline.HandleShard(ctx, sess, userMsg, shardTarget.Overrides)
	} else {
		response, info, err = a.pipeline.Handle(ctx, sess, userMsg, nil)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "completion failed: "+err.Error())
		return
	}
	resp := ChatResponse{
		SessionID: sess.ID,
		Content:   response,
	}
	if info != nil {
		resp.ModelID = info.ModelID
		resp.MemHits = info.MemHits
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Familiar-Session-ID", sess.ID)
	w.Header().Set("Access-Control-Expose-Headers", "X-Familiar-Session-ID")
	json.NewEncoder(w).Encode(resp)
}

func (a *Adapter) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "familiar-gateway",
	})
}

// handleTitle generates a short title for a new chat from its opening
// exchange. POST /api/chat/title with {user_message, assistant_message}
// → {title}. The workspace chat surface calls this once, at the end of
// a new conversation's first turn.
//
// Soft-fail by design: when the sidecar is down or unconfigured the
// handler returns {"title":""} with 200 so the frontend silently keeps
// its prompt-derived title. The endpoint touches no user-scoped data
// (pure text → text), but it IS LLM compute, so it requires the same
// session-cookie auth as /api/chat — otherwise it's a free,
// unauthenticated inference endpoint (cost / DoS vector).
// serveMemEvents gates the memlog SSE stream behind the same fail-closed
// session-cookie auth as /api/chat, plus an ownership check: the
// {session_id} is the conversation UUID, so a caller may only subscribe
// to a conversation they own. Without this the endpoint streamed any
// session's memory-write events to anyone who knew (or guessed) the UUID.
func (a *Adapter) serveMemEvents(w http.ResponseWriter, r *http.Request) {
	uid, authed := "", false
	if a.sessionReader != nil {
		uid, authed = a.sessionReader.UserIDFromRequest(r)
	}
	if !authed {
		writeError(w, http.StatusUnauthorized, "no user identity: authenticate via the workspace session")
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing session_id")
		return
	}
	// Ownership: the stream's session id is the conversation id. A
	// non-owner (or a non-conversation session id) gets a uniform 404 so
	// the endpoint doesn't confirm which ids exist.
	if a.convOwner != nil {
		owned, err := a.convOwner.OwnsConversation(r.Context(), sessionID, uid)
		if err != nil || !owned {
			http.NotFound(w, r)
			return
		}
	} else {
		// No ownership oracle wired — fail closed rather than leak.
		http.NotFound(w, r)
		return
	}
	a.memEvents.ServeSSE(w, r)
}

func (a *Adapter) handleTitle(w http.ResponseWriter, r *http.Request) {
	// Same fail-closed auth as /api/chat: a valid session cookie is
	// required. No sessionReader wired (or an invalid cookie) → 401.
	authed := false
	if a.sessionReader != nil {
		_, authed = a.sessionReader.UserIDFromRequest(r)
	}
	if !authed {
		writeError(w, http.StatusUnauthorized, "no user identity: authenticate via the workspace session")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	var req struct {
		UserMessage      string `json:"user_message"`
		AssistantMessage string `json:"assistant_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	req.UserMessage = strings.TrimSpace(req.UserMessage)
	if req.UserMessage == "" {
		writeError(w, http.StatusBadRequest, "user_message is required")
		return
	}

	title := ""
	if a.pipeline != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		t, err := a.pipeline.GenerateTitle(ctx, req.UserMessage, req.AssistantMessage)
		if err != nil {
			log.Printf("[http] autotitle: %v (frontend keeps derived title)", err)
		} else {
			title = t
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title})
}

// -- middleware --

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[http] %s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// -- helpers --

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"code":    status,
		},
	})
}
