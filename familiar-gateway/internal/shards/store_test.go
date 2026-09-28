package shards

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/testutil"
)

// These tests opt in via FAMILIAR_TEST_DSN. They share a single test
// database and truncate the relevant tables at setup to avoid bleed
// across runs. See internal/testutil for the skip/connect policy.
//
// The tests use a fixed test owner ("owner"); the `users` row is
// created by the gateway's own migrations (users_backfill handles
// deploys that predate the table), but we also ensure it exists here
// so a fresh test DB doesn't fail on the FK.

func setupShardStore(t *testing.T) *PGStore {
	t.Helper()
	pool := testutil.PgTestPool(t)
	// Clear prior rows. Every table that FK-references shards must
	// ride along (shard_passkeys, scheduled_actions + its runs,
	// shard_skills): TRUNCATE refuses a referenced table unless every
	// referencing table is in the same statement.
	testutil.TruncateTables(t, pool,
		"scheduled_action_runs", "scheduled_actions",
		"shard_skills", "shard_passkeys", "shard_tokens", "shards")
	// Make sure the FK-referenced owner exists.
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('owner', 'Test Owner', 'approved', 'admin')
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func validShard(id string) *Shard {
	return &Shard{
		ID:            id,
		OwnerID:       "owner",
		Name:          "Test " + id,
		Description:   "desc",
		Persistence:   PersistencePersistent,
		Visibility:    VisibilityIsolated,
		ScopeTag:      "shard:" + id,
		ToolAllowlist: []string{"web_search"},
		SystemPrompt:  "you are a test shard",
		MaxTokens:     1024,
		Temperature:   0.7,
	}
}

// -----------------------------------------------------------------------------
// Pure (non-DB) validation
// -----------------------------------------------------------------------------

func TestValidateSlug(t *testing.T) {
	cases := []struct {
		slug string
		ok   bool
	}{
		{"ticket-triage", true},
		{"x", true},
		{"0abc", true},
		{"abc123", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{"Ticket", false},
		{"-leading-hyphen", false},
		{"has_underscore", false},
		{strings.Repeat("a", 64), false},
	}
	for _, c := range cases {
		err := ValidateSlug(c.slug)
		if c.ok && err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", c.slug, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", c.slug)
		}
	}
}

func TestValidateAllowlist_EphemeralRejectsWriteTools(t *testing.T) {
	for _, write := range []string{"save_fact", "remember", "forget_fact", "correct_fact"} {
		err := ValidateAllowlist([]string{"web_search", write}, PersistenceEphemeral, nil)
		if !errors.Is(err, ErrWriteToolOnEph) {
			t.Errorf("ephemeral+%s: err = %v, want ErrWriteToolOnEph", write, err)
		}
	}
}

func TestValidateAllowlist_PersistentAllowsWriteTools(t *testing.T) {
	err := ValidateAllowlist([]string{"remember"}, PersistencePersistent, nil)
	if err != nil {
		t.Errorf("persistent+write: err = %v, want nil", err)
	}
}

func TestValidateAllowlist_EphemeralAllowsReadOnlyMemoryTools(t *testing.T) {
	err := ValidateAllowlist([]string{"search_memory", "list_my_memories"}, PersistenceEphemeral, nil)
	if err != nil {
		t.Errorf("ephemeral+read-only memory: err = %v, want nil", err)
	}
}

func TestValidateAllowlist_Duplicates(t *testing.T) {
	err := ValidateAllowlist([]string{"web_search", "web_search"}, PersistencePersistent, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicates: err = %v, want duplicate error", err)
	}
}

func TestValidateAllowlist_UnknownAgainstRegistry(t *testing.T) {
	known := map[string]bool{"web_search": true}
	err := ValidateAllowlist([]string{"web_search", "gopher_search"}, PersistencePersistent, known)
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("unknown tool: err = %v, want ErrUnknownTool", err)
	}
}

// TestValidateAllowlist_DottedNameHint covers the diagnostic the
// findings doc asks for: a SQL/script author who copies the older
// `<skill>.<tool>` examples gets a targeted hint pointing at the
// bare form, not just a generic "unknown tool" error.
func TestValidateAllowlist_DottedNameHint(t *testing.T) {
	known := map[string]bool{"save_fact": true}
	err := ValidateAllowlist([]string{"memory.save_fact"}, PersistencePersistent, known)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "save_fact") || !strings.Contains(msg, "bare") {
		t.Errorf("hint message should suggest the bare name (got %q)", msg)
	}
}

// -----------------------------------------------------------------------------
// Shard CRUD
// -----------------------------------------------------------------------------

func TestShard_CreateAndGet(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	sh := validShard("triage")
	sh.InputSchema = json.RawMessage(`{"type":"object"}`)

	if err := s.CreateShard(ctx, sh); err != nil {
		t.Fatalf("CreateShard: %v", err)
	}
	got, err := s.GetShard(ctx, "triage")
	if err != nil {
		t.Fatalf("GetShard: %v", err)
	}
	if got.Name != sh.Name {
		t.Errorf("name = %q, want %q", got.Name, sh.Name)
	}
	if got.Persistence != PersistencePersistent {
		t.Errorf("persistence = %q", got.Persistence)
	}
	if len(got.ToolAllowlist) != 1 || got.ToolAllowlist[0] != "web_search" {
		t.Errorf("allowlist = %v", got.ToolAllowlist)
	}
	// jsonb normalizes formatting (adds a space after the colon), so
	// compare compacted bytes rather than the literal input.
	var compact bytes.Buffer
	if err := json.Compact(&compact, got.InputSchema); err != nil {
		t.Fatalf("compact input_schema: %v", err)
	}
	if compact.String() != `{"type":"object"}` {
		t.Errorf("input_schema round-trip = %s", got.InputSchema)
	}
	if !got.Active() {
		t.Errorf("new shard should be active")
	}
}

func TestShard_CreateRejectsBadSlug(t *testing.T) {
	s := setupShardStore(t)
	sh := validShard("BadSlug")
	if err := s.CreateShard(context.Background(), sh); !errors.Is(err, ErrInvalidSlug) {
		t.Errorf("err = %v, want ErrInvalidSlug", err)
	}
}

func TestShard_CreateRejectsEphemeralWriteTool(t *testing.T) {
	s := setupShardStore(t)
	sh := validShard("eph")
	// Bare registry name — the dotted <skill>.<tool> form is a display
	// convention; the write-capable map (like the registry) keys on
	// bare names.
	sh.Persistence = PersistenceEphemeral
	sh.ToolAllowlist = []string{"remember"}
	if err := s.CreateShard(context.Background(), sh); !errors.Is(err, ErrWriteToolOnEph) {
		t.Errorf("err = %v, want ErrWriteToolOnEph", err)
	}
}

func TestShard_CreateRejectsBothModelAndTier(t *testing.T) {
	s := setupShardStore(t)
	sh := validShard("both")
	sh.ModelPreference = "gpu-host/x"
	sh.TierPreference = "technical"
	err := s.CreateShard(context.Background(), sh)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want mutually-exclusive error", err)
	}
}

func TestShard_CreateEnforcesScopeUniquePerOwner(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	a := validShard("alpha")
	b := validShard("beta")
	b.ScopeTag = a.ScopeTag // collision
	if err := s.CreateShard(ctx, a); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := s.CreateShard(ctx, b); err == nil {
		t.Errorf("expected unique-constraint violation on duplicate scope_tag")
	}
}

func TestShard_GetMissing(t *testing.T) {
	s := setupShardStore(t)
	if _, err := s.GetShard(context.Background(), "nope"); !errors.Is(err, ErrShardNotFound) {
		t.Errorf("err = %v, want ErrShardNotFound", err)
	}
}

func TestShard_ListOrdersNewestFirst(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.CreateShard(ctx, validShard(id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	list, err := s.ListShards(ctx, "owner")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Newest first: c, b, a.
	if list[0].ID != "c" || list[2].ID != "a" {
		t.Errorf("order = %s, %s, %s; want c, b, a", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestShard_Update(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	sh := validShard("u")
	if err := s.CreateShard(ctx, sh); err != nil {
		t.Fatal(err)
	}
	sh.Name = "Renamed"
	sh.ToolAllowlist = []string{} // no tools
	if err := s.UpdateShard(ctx, sh); err != nil {
		t.Fatalf("UpdateShard: %v", err)
	}
	got, err := s.GetShard(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.ToolAllowlist) != 0 {
		t.Errorf("allowlist = %v, want empty", got.ToolAllowlist)
	}
}

func TestShard_UpdateMissing(t *testing.T) {
	s := setupShardStore(t)
	sh := validShard("ghost")
	if err := s.UpdateShard(context.Background(), sh); !errors.Is(err, ErrShardNotFound) {
		t.Errorf("err = %v, want ErrShardNotFound", err)
	}
}

func TestShard_DisableEnableCycle(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	sh := validShard("toggle")
	if err := s.CreateShard(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableShard(ctx, "toggle"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	got, _ := s.GetShard(ctx, "toggle")
	if got.Active() {
		t.Errorf("should be disabled")
	}
	if err := s.EnableShard(ctx, "toggle"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	got, _ = s.GetShard(ctx, "toggle")
	if !got.Active() {
		t.Errorf("should be re-enabled")
	}
}

func TestShard_DeleteCascadesTokens(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("doomed")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, "owner", "doomed", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteShard(ctx, "doomed"); err != nil {
		t.Fatalf("DeleteShard: %v", err)
	}
	if toks, _ := s.ListTokens(ctx, "doomed"); len(toks) != 0 {
		t.Errorf("tokens after cascade = %d, want 0", len(toks))
	}
}

// -----------------------------------------------------------------------------
// Tokens
// -----------------------------------------------------------------------------

func TestToken_MintReturnsPlaintextOnce(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("mint")); err != nil {
		t.Fatal(err)
	}
	plaintext, tok, err := s.CreateToken(ctx, "owner", "mint", "cannonball")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(plaintext, TokenPlaintextPrefix) {
		t.Errorf("plaintext = %q, want prefix %q", plaintext, TokenPlaintextPrefix)
	}
	if len(plaintext) < tokenPrefixLen+16 {
		t.Errorf("plaintext length = %d, looks too short", len(plaintext))
	}
	if tok.TokenPrefix != plaintext[:tokenPrefixLen] {
		t.Errorf("prefix mismatch: %q vs %q", tok.TokenPrefix, plaintext[:tokenPrefixLen])
	}
	if tok.Label != "cannonball" {
		t.Errorf("label = %q", tok.Label)
	}
}

func TestToken_ValidateRoundTrip(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("rt")); err != nil {
		t.Fatal(err)
	}
	plaintext, tok, err := s.CreateToken(ctx, "owner", "rt", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ValidateToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("id = %q, want %q", got.ID, tok.ID)
	}
	if got.ShardID != "rt" {
		t.Errorf("shard_id = %q", got.ShardID)
	}
}

func TestToken_ValidateUnknownReturnsNotFound(t *testing.T) {
	s := setupShardStore(t)
	_, err := s.ValidateToken(context.Background(), "shard_notarealvaluedontvalidate")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestToken_ValidateShortReturnsNotFound(t *testing.T) {
	s := setupShardStore(t)
	// Fewer chars than tokenPrefixLen — should not hit the DB.
	_, err := s.ValidateToken(context.Background(), "abc")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestToken_ValidateRevokedReturnsRevoked(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("rv")); err != nil {
		t.Fatal(err)
	}
	plaintext, tok, err := s.CreateToken(ctx, "owner", "rv", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	_, err = s.ValidateToken(ctx, plaintext)
	if !errors.Is(err, ErrTokenRevoked) {
		t.Errorf("err = %v, want ErrTokenRevoked", err)
	}
}

func TestToken_RevokeIdempotent(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("ri")); err != nil {
		t.Fatal(err)
	}
	_, tok, err := s.CreateToken(ctx, "owner", "ri", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Errorf("second revoke: %v, want nil (idempotent)", err)
	}
}

func TestToken_RevokeMissing(t *testing.T) {
	s := setupShardStore(t)
	if err := s.RevokeToken(context.Background(), "no-such-id"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestToken_TouchUpdatesLastUsedAt(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("tu")); err != nil {
		t.Fatal(err)
	}
	_, tok, err := s.CreateToken(ctx, "owner", "tu", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TouchToken(ctx, tok.ID); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}
	list, err := s.ListTokens(ctx, "tu")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].LastUsedAt == nil {
		t.Errorf("last_used_at is still nil after touch")
	}
}

func TestToken_ListIncludesRevoked(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("hist")); err != nil {
		t.Fatal(err)
	}
	_, a, err := s.CreateToken(ctx, "owner", "hist", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, "owner", "hist", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTokens(ctx, "hist")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("len = %d, want 2 (active + revoked)", len(list))
	}
}

func TestToken_CreateRejectsOwnerMismatch(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	if err := s.CreateShard(ctx, validShard("guarded")); err != nil {
		t.Fatal(err)
	}
	// Seed a second user so the FK on owner_id is satisfied even though
	// the shard-owner check should reject first.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ('intruder', 'Intruder', 'approved', 'user')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateToken(ctx, "intruder", "guarded", "")
	if err == nil || !strings.Contains(err.Error(), "owner mismatch") {
		t.Errorf("err = %v, want owner-mismatch error", err)
	}
}

func TestToken_CreateForMissingShard(t *testing.T) {
	s := setupShardStore(t)
	_, _, err := s.CreateToken(context.Background(), "owner", "ghost", "")
	if !errors.Is(err, ErrShardNotFound) {
		t.Errorf("err = %v, want ErrShardNotFound", err)
	}
}

// -----------------------------------------------------------------------------
// Plaintext generator
// -----------------------------------------------------------------------------

func TestGenerateTokenPlaintext_ShapeAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		p, err := generateTokenPlaintext()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(p, TokenPlaintextPrefix) {
			t.Errorf("missing prefix: %q", p)
		}
		if len(p) != len(TokenPlaintextPrefix)+43 { // base64url no-pad of 32 bytes = 43 chars
			t.Errorf("len = %d, want %d", len(p), len(TokenPlaintextPrefix)+43)
		}
		if seen[p] {
			t.Fatalf("duplicate plaintext from rand: %q", p)
		}
		seen[p] = true
	}
}

// Deleting an ISOLATED shard deletes its private memory. Isolation is
// decided by a shards row matching the tag, so rows left behind became
// the owner's top-level memory once the shard was gone.
func TestShard_DeleteIsolatedPurgesItsMemory(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	iso := validShard("inbox")
	if err := s.CreateShard(ctx, iso); err != nil {
		t.Fatalf("CreateShard: %v", err)
	}
	promo := validShard("public")
	promo.Visibility = VisibilityPromoted
	if err := s.CreateShard(ctx, promo); err != nil {
		t.Fatalf("CreateShard: %v", err)
	}
	seed := func(content, tag string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO memories (agent_id, scope, content, source_type, user_id, scope_tag)
			 VALUES ('test', 'user', $1, 'explicit', 'owner', $2)`, content, tag); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO relationships (subject, predicate, object, user_id, scope_tag)
			 VALUES ($1, 'says', 'x', 'owner', $2)`, content, tag); err != nil {
			t.Fatalf("seed triple: %v", err)
		}
	}
	seed("injected instruction from an email", "shard:inbox")
	seed("public fact", "shard:public")
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM memories WHERE user_id = 'owner' AND scope_tag IN ('shard:inbox','shard:public')`)
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM relationships WHERE user_id = 'owner' AND scope_tag IN ('shard:inbox','shard:public')`)
	})

	for _, id := range []string{"inbox", "public"} {
		if err := s.DeleteShard(ctx, id); err != nil {
			t.Fatalf("DeleteShard %s: %v", id, err)
		}
	}
	count := func(table, tag string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE user_id = 'owner' AND scope_tag = $1`, tag).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count("memories", "shard:inbox") + count("relationships", "shard:inbox"); n != 0 {
		t.Errorf("isolated shard's memory survived its deletion (%d rows)", n)
	}
	if n := count("memories", "shard:public") + count("relationships", "shard:public"); n != 2 {
		t.Errorf("promoted shard's rows = %d, want 2 kept", n)
	}
}

// Renaming a shard's scope tag carries its rows along; otherwise they
// matched no shard and an isolated shard's rows surfaced top-level.
func TestShard_ScopeTagRenameRetagsMemory(t *testing.T) {
	s := setupShardStore(t)
	ctx := context.Background()
	sh := validShard("hr")
	if err := s.CreateShard(ctx, sh); err != nil {
		t.Fatalf("CreateShard: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (agent_id, scope, content, content_hash, source_type, user_id, scope_tag)
		 VALUES ('test', 'user', 'salary review notes', $1, 'explicit', 'owner', 'shard:hr')`,
		memory.FactHash("owner", "shard:hr", "explicit", "", "salary review notes")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM memories WHERE user_id = 'owner' AND scope_tag IN ('shard:hr','hr-v2')`)
	})
	sh.ScopeTag = "hr-v2"
	if err := s.UpdateShard(ctx, sh); err != nil {
		t.Fatalf("UpdateShard: %v", err)
	}
	var old, renamed int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE user_id='owner' AND scope_tag='shard:hr'`).Scan(&old)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE user_id='owner' AND scope_tag='hr-v2'`).Scan(&renamed)
	if old != 0 || renamed != 1 {
		t.Errorf("after rename: %d rows under the old tag, %d under the new; want 0 and 1", old, renamed)
	}
	// The scope is part of the row's dedup identity, so it is rehashed:
	// the shard restating the note lands on this row, not a second one.
	var hash string
	_ = s.db.QueryRowContext(ctx, `SELECT content_hash FROM memories WHERE user_id='owner' AND scope_tag='hr-v2'`).Scan(&hash)
	if want := memory.FactHash("owner", "hr-v2", "explicit", "", "salary review notes"); hash != want {
		t.Errorf("retagged row kept the old scope's hash %q, want %q", hash, want)
	}
}

func TestValidateScopeTag_ReservesBookPrefix(t *testing.T) {
	if err := ValidateScopeTag("book:3f2a"); err == nil {
		t.Error("a shard may not take a wiki book's scope tag")
	}
	if err := ValidateScopeTag("shard:kitchen"); err != nil {
		t.Errorf("ordinary tag rejected: %v", err)
	}
}
