package shardapi

// Handler-level tests: exercise auth correctness, body parsing, and
// response shape against a fake shards.Store + fake UserLookup + fake
// Pipeline. No DB, no real LLM. The shards store tests already cover
// persistence/token crypto in internal/shards; here we focus on the
// HTTP behavior the spec requires (401 / 403 / 404 / 410 / 422
// mapping, shard envelope, ignored body fields).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore is a minimal in-memory shards.Store for the handler's
// ValidateToken / GetShard / TouchToken code paths. Only the methods
// the shardapi handler actually calls are implemented; the rest panic
// so accidental new call sites fail loudly.
type fakeStore struct {
	mu      sync.Mutex
	shards  map[string]*shards.Shard
	tokens  map[string]*shards.Token // keyed by plaintext for easy lookup
	byID    map[string]*shards.Token // keyed by token id for TouchToken/Revoke
	touched []string                 // token IDs that got TouchToken
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		shards: make(map[string]*shards.Shard),
		tokens: make(map[string]*shards.Token),
		byID:   make(map[string]*shards.Token),
	}
}

func (f *fakeStore) addShard(s *shards.Shard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shards[s.ID] = s
}

func (f *fakeStore) addToken(plaintext string, t *shards.Token) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[plaintext] = t
	f.byID[t.ID] = t
}

func (f *fakeStore) GetShard(ctx context.Context, id string) (*shards.Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shards[id]
	if !ok {
		return nil, shards.ErrShardNotFound
	}
	// Return a defensive copy so handler mutations don't leak.
	cp := *s
	return &cp, nil
}

func (f *fakeStore) ValidateToken(ctx context.Context, plaintext string) (*shards.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[plaintext]
	if !ok {
		return nil, shards.ErrTokenNotFound
	}
	if t.RevokedAt != nil {
		return nil, shards.ErrTokenRevoked
	}
	cp := *t
	return &cp, nil
}

func (f *fakeStore) TouchToken(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

// Unused by the handler but required by the Store interface.
func (f *fakeStore) CreateShard(ctx context.Context, s *shards.Shard) error {
	panic("CreateShard not used")
}
func (f *fakeStore) ListShards(ctx context.Context, ownerID string) ([]*shards.Shard, error) {
	panic("ListShards not used")
}
func (f *fakeStore) ListAllShards(ctx context.Context) ([]*shards.Shard, error) {
	panic("ListAllShards not used")
}
func (f *fakeStore) UpdateShard(ctx context.Context, s *shards.Shard) error {
	panic("UpdateShard not used")
}
func (f *fakeStore) DisableShard(ctx context.Context, id string) error {
	panic("DisableShard not used")
}
func (f *fakeStore) EnableShard(ctx context.Context, id string) error { panic("EnableShard not used") }
func (f *fakeStore) DeleteShard(ctx context.Context, id string) error { panic("DeleteShard not used") }
func (f *fakeStore) CreateToken(ctx context.Context, ownerID, shardID, label string) (string, *shards.Token, error) {
	panic("CreateToken not used")
}
func (f *fakeStore) ListTokens(ctx context.Context, shardID string) ([]*shards.Token, error) {
	panic("ListTokens not used")
}
func (f *fakeStore) RevokeToken(ctx context.Context, id string) error { panic("RevokeToken not used") }

// fakeUsers stubs UserLookup. Missing emails return nil (not an
// error), mirroring the real GetByEmail contract.
type fakeUsers struct {
	byEmail map[string]*identity.User
}

func (f *fakeUsers) GetByEmail(ctx context.Context, email string) (*identity.User, error) {
	u, ok := f.byEmail[email]
	if !ok {
		return nil, nil
	}
	return u, nil
}

// fakePipeline short-circuits the pipeline; just records what it was
// invoked with so tests can assert, then returns canned text.
type fakePipeline struct {
	mu            sync.Mutex
	lastUserMsg   string
	lastOverrides *pipeline.ShardOverrides
	lastSess      *session.Session
	reply         string
	err           error
	calls         int
}

func (p *fakePipeline) HandleShard(ctx context.Context, sess *session.Session, userMsg string, overrides *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
	p.mu.Lock()
	p.calls++
	p.lastUserMsg = userMsg
	p.lastOverrides = overrides
	p.lastSess = sess
	p.mu.Unlock()
	if p.err != nil {
		return "", nil, p.err
	}
	return p.reply, &pipeline.RouteInfo{ModelID: "fake-model"}, nil
}

func (p *fakePipeline) HandleShardStream(
	ctx context.Context,
	sess *session.Session,
	userMsg string,
	overrides *pipeline.ShardOverrides,
	onChunk func(string),
	_ func(string),
	_ func(string),
) (string, *pipeline.RouteInfo, error) {
	p.mu.Lock()
	p.calls++
	p.lastUserMsg = userMsg
	p.lastOverrides = overrides
	p.lastSess = sess
	p.mu.Unlock()
	if p.err != nil {
		return "", nil, p.err
	}
	if onChunk != nil {
		onChunk(p.reply)
	}
	return p.reply, &pipeline.RouteInfo{ModelID: "fake-model"}, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	testEmail  = "owner@example.com"
	testOwner  = "owner"
	testShard  = "charger-extractor"
	testToken  = "shard_Xyz12abcdefghijklmnopqrstuvwxyz0123456789"
	testPrefix = "shard_X"
)

func approvedUser(id, email string) *identity.User {
	e := email
	return &identity.User{
		ID:     id,
		Email:  &e,
		Status: identity.StatusApproved,
		Role:   "user",
	}
}

func buildFixtures(t *testing.T) (*fakeStore, *fakeUsers, *fakePipeline, *Handler) {
	t.Helper()
	st := newFakeStore()
	st.addShard(&shards.Shard{
		ID:            testShard,
		OwnerID:       testOwner,
		Name:          "Charger Extractor",
		Persistence:   shards.PersistenceEphemeral,
		Visibility:    shards.VisibilityIsolated,
		ScopeTag:      "shard:charger-extractor",
		SystemPrompt:  "extract charger metadata, return JSON only",
		ToolAllowlist: []string{"web_search"},
		MaxTokens:     2048,
		Temperature:   0.1,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	})
	st.addToken(testToken, &shards.Token{
		ID:          "tok-1",
		ShardID:     testShard,
		OwnerID:     testOwner,
		TokenPrefix: testPrefix,
		CreatedAt:   time.Now(),
	})
	users := &fakeUsers{byEmail: map[string]*identity.User{
		testEmail: approvedUser(testOwner, testEmail),
	}}
	pipe := &fakePipeline{reply: `{"name":"Electrify America Fresno"}`}
	h := New(st, pipe, session.NewManager(), users)
	return st, users, pipe, h
}

func doInvoke(t *testing.T, h *Handler, shardID, email, token, bodyJSON string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/shards/"+shardID+"/invoke",
		strings.NewReader(bodyJSON))
	if email != "" {
		req.Header.Set("X-Familiar-User-Email", email)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeErr(t *testing.T, body []byte) string {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error response: %v (body=%s)", err, string(body))
	}
	return e.Error
}

const validBody = `{"messages":[{"role":"user","content":"go"}]}`

// ---------------------------------------------------------------------------
// Auth failures — each one verifies the status code AND that the
// pipeline was NOT invoked (no leak past the auth boundary).
// ---------------------------------------------------------------------------

func TestInvoke_MissingEmailHeader401(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, "", testToken, validBody)
	if rr.Code != 401 {
		t.Errorf("code = %d, want 401", rr.Code)
	}
	if !strings.Contains(decodeErr(t, rr.Body.Bytes()), "X-Familiar-User-Email") {
		t.Errorf("error message should name the missing header")
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run on auth failure")
	}
}

func TestInvoke_MissingBearer401(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, "", validBody)
	if rr.Code != 401 {
		t.Errorf("code = %d, want 401", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_UnknownToken401(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, "shard_notavalidtoken0000000000000000000000000", validBody)
	if rr.Code != 401 {
		t.Errorf("code = %d, want 401", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_RevokedToken403(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	// Revoke the fixture token.
	st.mu.Lock()
	now := time.Now()
	st.tokens[testToken].RevokedAt = &now
	st.mu.Unlock()
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_TokenShardMismatch403(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	// Add a second shard the token does NOT belong to.
	st.addShard(&shards.Shard{
		ID:           "other-shard",
		OwnerID:      testOwner,
		Persistence:  shards.PersistenceEphemeral,
		Visibility:   shards.VisibilityIsolated,
		ScopeTag:     "shard:other",
		SystemPrompt: "x",
		MaxTokens:    1024,
	})
	rr := doInvoke(t, h, "other-shard", testEmail, testToken, validBody)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	if !strings.Contains(decodeErr(t, rr.Body.Bytes()), "not valid for this shard") {
		t.Errorf("expected shard-mismatch message")
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_EmailNotRegistered403(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, "stranger@example.com", testToken, validBody)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_EmailOwnerMismatch403(t *testing.T) {
	_, users, pipe, h := buildFixtures(t)
	// Register a second user whose email doesn't own the fixture token.
	users.byEmail["other@example.com"] = approvedUser("other-user", "other@example.com")
	rr := doInvoke(t, h, testShard, "other@example.com", testToken, validBody)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_UserNotApproved403(t *testing.T) {
	_, users, pipe, h := buildFixtures(t)
	users.byEmail[testEmail].Status = identity.StatusPending
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_ShardNotFound404(t *testing.T) {
	// URL shard id exists in the token validation path (token still
	// validates), but the shard row is gone. To simulate: mint a token
	// for a shard that doesn't exist. We use the fakeStore's addToken
	// directly.
	_, _, pipe, _ := buildFixtures(t)
	h2Store := newFakeStore()
	h2Store.addToken(testToken, &shards.Token{
		ID: "tok-phantom", ShardID: "phantom", OwnerID: testOwner,
		TokenPrefix: testPrefix, CreatedAt: time.Now(),
	})
	users := &fakeUsers{byEmail: map[string]*identity.User{testEmail: approvedUser(testOwner, testEmail)}}
	h2 := New(h2Store, pipe, session.NewManager(), users)
	rr := doInvoke(t, h2, "phantom", testEmail, testToken, validBody)
	if rr.Code != 404 {
		t.Errorf("code = %d, want 404", rr.Code)
	}
}

func TestInvoke_DisabledShard410(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	now := time.Now()
	st.mu.Lock()
	st.shards[testShard].DisabledAt = &now
	st.mu.Unlock()
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 410 {
		t.Errorf("code = %d, want 410", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

// ---------------------------------------------------------------------------
// Body parsing
// ---------------------------------------------------------------------------

func TestInvoke_MalformedBody422(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, testToken, `{"messages": not-json}`)
	if rr.Code != 422 {
		t.Errorf("code = %d, want 422", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run on malformed body")
	}
}

func TestInvoke_NoUserMessage422(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, testToken, `{"messages":[{"role":"system","content":"x"}]}`)
	if rr.Code != 422 {
		t.Errorf("code = %d, want 422", rr.Code)
	}
	if pipe.calls != 0 {
		t.Errorf("pipeline should not run")
	}
}

func TestInvoke_MethodNotAllowed(t *testing.T) {
	_, _, _, h := buildFixtures(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/"+testShard+"/invoke", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// ServeMux returns 405 for method mismatch on a registered pattern.
	if rr.Code != 405 {
		t.Errorf("GET on POST-only route: code = %d, want 405", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Happy path — end-to-end shape
// ---------------------------------------------------------------------------

func TestInvoke_NonStreamingOK(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 200 {
		t.Fatalf("code = %d (%s), want 200", rr.Code, rr.Body.String())
	}

	var resp invokeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Shard.ID != testShard {
		t.Errorf("shard.id = %q, want %q", resp.Shard.ID, testShard)
	}
	// Ephemeral shard: no session_id exposed.
	if resp.Shard.SessionID != "" {
		t.Errorf("ephemeral shard should expose empty session_id, got %q", resp.Shard.SessionID)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("unexpected choices: %+v", resp.Choices)
	}
	if resp.Choices[0].Message.Content != pipe.reply {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, pipe.reply)
	}

	// Pipeline was invoked with the shard's system prompt + scope tag.
	if pipe.lastOverrides == nil {
		t.Fatal("overrides not passed to pipeline")
	}
	if pipe.lastOverrides.SystemPrompt != "extract charger metadata, return JSON only" {
		t.Errorf("system prompt not propagated: %q", pipe.lastOverrides.SystemPrompt)
	}
	if pipe.lastOverrides.ScopeTag != "shard:charger-extractor" {
		t.Errorf("scope_tag not propagated: %q", pipe.lastOverrides.ScopeTag)
	}
	if !pipe.lastOverrides.SkipCommit {
		t.Errorf("ephemeral shard must set SkipCommit")
	}
	if !pipe.lastOverrides.SkipSessionHydration {
		t.Errorf("ephemeral shard must set SkipSessionHydration")
	}
	if len(pipe.lastOverrides.ToolAllowlist) != 1 || pipe.lastOverrides.ToolAllowlist[0] != "web_search" {
		t.Errorf("allowlist not propagated: %v", pipe.lastOverrides.ToolAllowlist)
	}
	if pipe.lastOverrides.Temperature == nil || *pipe.lastOverrides.Temperature != 0.1 {
		t.Errorf("temperature not propagated: %v", pipe.lastOverrides.Temperature)
	}

	// TouchToken runs async; give it a brief moment then check.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		n := len(st.touched)
		st.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.touched) == 0 || st.touched[0] != "tok-1" {
		t.Errorf("expected TouchToken(tok-1), got %v", st.touched)
	}
}

func TestInvoke_IgnoresCallerModelTempMaxTokens(t *testing.T) {
	// The spec explicitly rejects expansion of the envelope via body
	// fields. Send a body that tries to override model / temperature /
	// max_tokens and verify the shard's values landed on the pipeline
	// instead.
	_, _, pipe, h := buildFixtures(t)
	body := `{
		"messages":[{"role":"user","content":"go"}],
		"model":"attacker-model",
		"temperature":0.99,
		"max_tokens":9999,
		"user":"spoofed@example.com"
	}`
	rr := doInvoke(t, h, testShard, testEmail, testToken, body)
	if rr.Code != 200 {
		t.Fatalf("code = %d (%s)", rr.Code, rr.Body.String())
	}
	if pipe.lastOverrides.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want shard's 2048 (caller 9999 ignored)", pipe.lastOverrides.MaxTokens)
	}
	if pipe.lastOverrides.Temperature == nil || *pipe.lastOverrides.Temperature != 0.1 {
		t.Errorf("temperature not locked to shard's 0.1")
	}
	// Sanity: the envelope model field reflects the shard, not the
	// caller's "attacker-model".
	var resp invokeResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Model == "attacker-model" {
		t.Errorf("response.model echoed caller value — capability envelope leaked")
	}
}

func TestInvoke_PersistentShardExposesSessionID(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	// Promote fixture shard to persistent.
	st.mu.Lock()
	st.shards[testShard].Persistence = shards.PersistencePersistent
	st.mu.Unlock()
	// Caller supplies a session_id — handler must echo it.
	body := `{"messages":[{"role":"user","content":"go"}],"session_id":"sess-abc"}`
	rr := doInvoke(t, h, testShard, testEmail, testToken, body)
	if rr.Code != 200 {
		t.Fatalf("code = %d (%s)", rr.Code, rr.Body.String())
	}
	var resp invokeResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Shard.SessionID != "sess-abc" {
		t.Errorf("session_id = %q, want sess-abc", resp.Shard.SessionID)
	}
	if pipe.lastOverrides.SkipCommit {
		t.Errorf("persistent shard must NOT set SkipCommit")
	}
}

func TestInvoke_Streaming200SSE(t *testing.T) {
	_, _, _, h := buildFixtures(t)
	body := `{"messages":[{"role":"user","content":"hi"}],"stream":true}`
	rr := doInvoke(t, h, testShard, testEmail, testToken, body)
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	bodyText := rr.Body.String()
	// The reply is JSON-escaped inside the SSE envelope; check for a
	// distinctive substring that survives escaping ("Electrify America
	// Fresno" contains no JSON-special chars).
	if !strings.Contains(bodyText, "Electrify America Fresno") {
		t.Errorf("streamed body missing reply text: %q", bodyText)
	}
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", bodyText)
	}
}

// ---------------------------------------------------------------------------
// Pipeline failure mapping
// ---------------------------------------------------------------------------

// TestInvoke_IsolatedShardSetsExcludeFromHot covers the
// FAMILIAR-SHARDS-PHASE1-FINDINGS Issue 3 wiring: an
// isolated-visibility shard's overrides flip ExcludeFromHot so any
// downstream commit (pipeline conversation fact, memory-skill writes)
// is routed past the engine's RAM tier. The fixture shard in
// buildFixtures is already visibility=isolated, so we just assert
// the flag landed; a parallel test flips it to promoted and asserts
// the inverse.
func TestInvoke_IsolatedShardSetsExcludeFromHot(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if pipe.lastOverrides == nil || !pipe.lastOverrides.ExcludeFromHot {
		t.Errorf("isolated shard overrides should set ExcludeFromHot=true; got %+v",
			pipe.lastOverrides)
	}
}

func TestInvoke_PromotedShardLeavesExcludeFromHotFalse(t *testing.T) {
	st, _, pipe, h := buildFixtures(t)
	st.mu.Lock()
	st.shards[testShard].Visibility = shards.VisibilityPromoted
	st.mu.Unlock()
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if pipe.lastOverrides == nil || pipe.lastOverrides.ExcludeFromHot {
		t.Errorf("promoted shard overrides should leave ExcludeFromHot=false; got %+v",
			pipe.lastOverrides)
	}
}

func TestInvoke_PipelineErrorReturns500(t *testing.T) {
	_, _, pipe, h := buildFixtures(t)
	pipe.err = errors.New("upstream LLM boom")
	rr := doInvoke(t, h, testShard, testEmail, testToken, validBody)
	if rr.Code != 500 {
		t.Errorf("code = %d, want 500", rr.Code)
	}
}

// Silences the unused imports for fmt/io when the test file's coverage
// grows or shrinks; no-op otherwise.
var _ = fmt.Sprintf
var _ = io.Discard
