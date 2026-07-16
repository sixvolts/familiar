package admin

// Role-gating tests for the Phase B shards reclassification:
//
//   - non-admin users see only their own shards
//   - non-admin users get 404 (not 403) on someone else's shard
//   - admin users see every shard on the instance
//   - shard creation forces owner_id from the session — even an admin
//     calling POST /console/api/shards creates a shard owned by
//     themselves; spoofing via body is impossible
//   - token mint stays strict per-shard-owner even for admins (the
//     resulting token would never authenticate against the wrong
//     owner anyway, so we 404 early)
//   - token revoke admits admins on any token; non-admins only own
//
// The fakeShardStore here implements just the methods the role-gated
// handlers exercise; everything else panics so accidental new call
// sites fail loud.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/shards"
)

// ──────────────────────────────────────────────────────────────────
// Fake ShardStore
// ──────────────────────────────────────────────────────────────────

type fakeShardStore struct {
	mu     sync.Mutex
	shards map[string]*shards.Shard
	tokens map[string][]*shards.Token // shardID → tokens
}

func newFakeShardStore() *fakeShardStore {
	return &fakeShardStore{
		shards: make(map[string]*shards.Shard),
		tokens: make(map[string][]*shards.Token),
	}
}

func (f *fakeShardStore) addShard(s *shards.Shard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shards[s.ID] = s
}

func (f *fakeShardStore) addToken(shardID string, t *shards.Token) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[shardID] = append(f.tokens[shardID], t)
}

func (f *fakeShardStore) GetShard(_ context.Context, id string) (*shards.Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.shards[id]
	if !ok {
		return nil, shards.ErrShardNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeShardStore) ListShards(_ context.Context, ownerID string) ([]*shards.Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*shards.Shard{}
	for _, s := range f.shards {
		if s.OwnerID == ownerID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeShardStore) ListAllShards(_ context.Context) ([]*shards.Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*shards.Shard, 0, len(f.shards))
	for _, s := range f.shards {
		cp := *s
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeShardStore) ListTokens(_ context.Context, shardID string) ([]*shards.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]*shards.Token(nil), f.tokens[shardID]...)
	return out, nil
}

func (f *fakeShardStore) CreateShard(_ context.Context, s *shards.Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *s
	cp.CreatedAt = time.Now()
	cp.UpdatedAt = cp.CreatedAt
	f.shards[s.ID] = &cp
	return nil
}

func (f *fakeShardStore) CreateToken(_ context.Context, ownerID, shardID, label string) (string, *shards.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := &shards.Token{
		ID:          "tok-" + shardID + "-" + label,
		ShardID:     shardID,
		OwnerID:     ownerID,
		Label:       label,
		TokenPrefix: "shard_X",
		CreatedAt:   time.Now(),
	}
	f.tokens[shardID] = append(f.tokens[shardID], tok)
	return "shard_PLAINTEXT", tok, nil
}

func (f *fakeShardStore) RevokeToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, toks := range f.tokens {
		for _, t := range toks {
			if t.ID == id {
				now := time.Now()
				t.RevokedAt = &now
				return nil
			}
		}
	}
	return shards.ErrTokenNotFound
}

// Unused by the role-gated handlers — panic on accidental use.
func (f *fakeShardStore) UpdateShard(context.Context, *shards.Shard) error {
	panic("UpdateShard not used")
}
func (f *fakeShardStore) DisableShard(context.Context, string) error { panic("DisableShard not used") }
func (f *fakeShardStore) EnableShard(context.Context, string) error  { panic("EnableShard not used") }
func (f *fakeShardStore) DeleteShard(context.Context, string) error  { panic("DeleteShard not used") }

// ──────────────────────────────────────────────────────────────────
// Fixtures
// ──────────────────────────────────────────────────────────────────

func makeShardsHandler(t *testing.T, store *fakeShardStore) *Handler {
	t.Helper()
	h := &Handler{}
	h.AttachShardStore(store)
	return h
}

func operatorAdmin() AuthUser { return AuthUser{UserID: "operator", Role: "admin"} }
func alisonUser() AuthUser    { return AuthUser{UserID: "alison", Role: "user"} }
func strangerUser() AuthUser  { return AuthUser{UserID: "stranger", Role: "user"} }

func operatorShard(id string) *shards.Shard {
	return &shards.Shard{
		ID: id, OwnerID: "operator", Name: "Operator " + id,
		Persistence: shards.PersistenceEphemeral, Visibility: shards.VisibilityIsolated,
		ScopeTag: "shard:" + id, SystemPrompt: "test", MaxTokens: 1024,
	}
}

func alisonShard(id string) *shards.Shard {
	return &shards.Shard{
		ID: id, OwnerID: "alison", Name: "Alison " + id,
		Persistence: shards.PersistenceEphemeral, Visibility: shards.VisibilityIsolated,
		ScopeTag: "shard:" + id, SystemPrompt: "test", MaxTokens: 1024,
	}
}

// listResponse matches the handler's wire shape closely enough for
// assertions; only `items[].owner_id` is needed here.
type listResponse struct {
	Items []struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
	} `json:"items"`
}

func decodeList(t *testing.T, body []byte) listResponse {
	t.Helper()
	var lr listResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, string(body))
	}
	return lr
}

// ──────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────

func TestListShards_AdminSeesAllInstanceShards(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(operatorShard("a-1"))
	store.addShard(alisonShard("l-1"))
	store.addShard(alisonShard("l-2"))
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("GET", "/console/api/shards", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.listShards(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeList(t, rec.Body.Bytes())
	if len(got.Items) != 3 {
		t.Errorf("admin list len = %d, want 3 (saw all instance shards)", len(got.Items))
	}
}

func TestListShards_UserSeesOnlyOwnShards(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(operatorShard("a-1"))
	store.addShard(alisonShard("l-1"))
	store.addShard(alisonShard("l-2"))
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("GET", "/console/api/shards", nil).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.listShards(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeList(t, rec.Body.Bytes())
	if len(got.Items) != 2 {
		t.Errorf("user list len = %d, want 2 (own only)", len(got.Items))
	}
	for _, s := range got.Items {
		if s.OwnerID != "alison" {
			t.Errorf("non-admin saw foreign shard %s owned by %s", s.ID, s.OwnerID)
		}
	}
}

func TestGetShard_AdminCanReadOthersShard(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(alisonShard("l-1"))
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("GET", "/console/api/shards/l-1", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	req.SetPathValue("id", "l-1")
	rec := httptest.NewRecorder()
	h.getShard(rec, req)
	if rec.Code != 200 {
		t.Errorf("admin get on other-user shard = %d, want 200", rec.Code)
	}
}

func TestGetShard_UserGets404OnOthersShard(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(alisonShard("l-1"))
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("GET", "/console/api/shards/l-1", nil).WithContext(
		ctxWithAuth(context.Background(), strangerUser()))
	req.SetPathValue("id", "l-1")
	rec := httptest.NewRecorder()
	h.getShard(rec, req)
	// 404 (not 403) — leaking existence would let a non-admin
	// enumerate UUIDs to discover other users' shard inventory.
	if rec.Code != 404 {
		t.Errorf("non-owner get = %d, want 404 (existence-hiding)", rec.Code)
	}
}

// TestShardCRUD_RefusesShardSession verifies a shard session is
// rejected with 403 from every shard CRUD handler. Without this gate
// a constrained shard could PATCH its own row and grant itself an
// unrestricted permission envelope. The handlers that mutate shard
// state must mirror the resolveOwnedShard pattern from the passkey
// surface.
func TestShardCRUD_RefusesShardSession(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(&shards.Shard{
		ID: "k", Name: "kiosk", OwnerID: "alice", Persistence: shards.PersistenceEphemeral,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := &Handler{shards: store}

	shardAU := AuthUser{
		UserID:        "alice",
		Role:          "user",
		PrincipalType: PrincipalTypeShard,
		ShardID:       "k",
	}

	cases := []struct {
		name   string
		method string
		path   string
		fn     http.HandlerFunc
		body   string
	}{
		{"listShards", "GET", "/shards", h.listShards, ""},
		{"getShard", "GET", "/shards/k", h.getShard, ""},
		{"createShard", "POST", "/shards", h.createShard, `{"id":"new","name":"escalated"}`},
		{"patchShard", "PATCH", "/shards/k", h.patchShard, `{"console_access":true,"book_access":[]}`},
		{"deleteShard", "DELETE", "/shards/k", h.deleteShard, ""},
		{"disableShard", "POST", "/shards/k/disable", h.disableShard, ""},
		{"enableShard", "POST", "/shards/k/enable", h.enableShard, ""},
		{"listShardTokens", "GET", "/shards/k/tokens", h.listShardTokens, ""},
		{"createShardToken", "POST", "/shards/k/tokens", h.createShardToken, `{}`},
		{"revokeShardToken", "DELETE", "/shards/k/tokens/t1", h.revokeShardToken, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req = req.WithContext(ctxWithAuth(req.Context(), shardAU))
			// Some handlers read PathValue; the http.NewServeMux
			// wiring isn't in scope here, but the refuse-gate fires
			// before any PathValue read so the test doesn't need it.
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (shard session must not manage shards)", rec.Code)
			}
		})
	}
}

func TestCreateShard_OwnerForcedFromSession(t *testing.T) {
	// Spec acceptance: "She cannot create a shard owned by Operator
	// (owner is forced from session user, not request body)."
	store := newFakeShardStore()
	h := makeShardsHandler(t, store)

	// Body claims owner = "operator", caller is alison.
	body := []byte(`{
        "id":"spoof",
        "name":"spoof",
        "persistence":"ephemeral",
        "visibility":"isolated",
        "scope_tag":"shard:spoof",
        "system_prompt":"test",
        "tool_allowlist":[],
        "max_tokens":1024,
        "owner_id":"operator"
    }`)
	req := httptest.NewRequest("POST", "/console/api/shards", bytes.NewReader(body)).WithContext(
		ctxWithAuth(context.Background(), alisonUser()))
	req = req.WithContext(context.WithValue(req.Context(), ContextKeyUserID, "alison"))
	rec := httptest.NewRecorder()
	h.createShard(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	stored := store.shards["spoof"]
	if stored == nil {
		t.Fatal("shard not stored")
	}
	if stored.OwnerID != "alison" {
		t.Errorf("owner_id = %q, want 'alison' (request body's 'operator' must be ignored)", stored.OwnerID)
	}
}

func TestCreateToken_AdminCannotMintAgainstOtherUsersShard(t *testing.T) {
	// Documented restriction: even admins can't mint tokens on
	// another user's shard. The minted token would never authenticate
	// (the shardapi auth path checks email == shard.owner ==
	// token.owner), so 404 is the honest answer.
	store := newFakeShardStore()
	store.addShard(alisonShard("l-1"))
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("POST", "/console/api/shards/l-1/tokens",
		bytes.NewReader([]byte(`{"label":"sneaky"}`))).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	req.SetPathValue("id", "l-1")
	req = req.WithContext(context.WithValue(req.Context(), ContextKeyUserID, "operator"))
	rec := httptest.NewRecorder()
	h.createShardToken(rec, req)

	if rec.Code != 404 {
		t.Errorf("admin minting on other user's shard = %d, want 404", rec.Code)
	}
	if len(store.tokens["l-1"]) != 0 {
		t.Errorf("token was created despite 404; count=%d", len(store.tokens["l-1"]))
	}
}

func TestRevokeToken_AdminCanRevokeAny(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(alisonShard("l-1"))
	store.addToken("l-1", &shards.Token{
		ID: "tok-alison-1", ShardID: "l-1", OwnerID: "alison",
		TokenPrefix: "shard_A", CreatedAt: time.Now(),
	})
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("POST", "/console/api/shard_tokens/tok-alison-1/revoke", nil).WithContext(
		ctxWithAuth(context.Background(), operatorAdmin()))
	req.SetPathValue("tid", "tok-alison-1")
	req = req.WithContext(context.WithValue(req.Context(), ContextKeyUserID, "operator"))
	rec := httptest.NewRecorder()
	h.revokeShardToken(rec, req)

	if rec.Code != 200 {
		t.Fatalf("admin revoke = %d (%s)", rec.Code, rec.Body.String())
	}
	if store.tokens["l-1"][0].RevokedAt == nil {
		t.Errorf("token was not revoked")
	}
}

func TestRevokeToken_UserCannotRevokeOthers(t *testing.T) {
	store := newFakeShardStore()
	store.addShard(alisonShard("l-1"))
	store.addToken("l-1", &shards.Token{
		ID: "tok-alison-1", ShardID: "l-1", OwnerID: "alison",
		TokenPrefix: "shard_A", CreatedAt: time.Now(),
	})
	h := makeShardsHandler(t, store)

	req := httptest.NewRequest("POST", "/console/api/shard_tokens/tok-alison-1/revoke", nil).WithContext(
		ctxWithAuth(context.Background(), strangerUser()))
	req.SetPathValue("tid", "tok-alison-1")
	req = req.WithContext(context.WithValue(req.Context(), ContextKeyUserID, "stranger"))
	rec := httptest.NewRecorder()
	h.revokeShardToken(rec, req)

	if rec.Code != 404 {
		t.Errorf("non-owner revoke = %d, want 404", rec.Code)
	}
	if store.tokens["l-1"][0].RevokedAt != nil {
		t.Errorf("token was revoked despite 404")
	}
}

// ──────────────────────────────────────────────────────────────────
// /admin/* → /console/* back-compat redirects
// ──────────────────────────────────────────────────────────────────

// /admin → /console redirect moved to familiar-workspace as part of
// FAMILIAR-WORKSPACE-SPEC Phase 0 (the gateway is API-only now;
// bookmarks land on the workspace's hostname). The corresponding
// regression test lives at familiar-workspace/cmd/workspace/main_test.go.
