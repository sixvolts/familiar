package session

import (
	"context"
	"testing"

	"github.com/familiar/gateway/internal/testutil"
)

// These tests opt in via FAMILIAR_TEST_DSN. They share a single test
// database and truncate the sessions table at setup to avoid bleed
// across runs. See internal/testutil for the skip/connect policy.

func setupSessionStore(t *testing.T) *Store {
	t.Helper()
	pool := testutil.PgTestPool(t)
	testutil.TruncateTables(t, pool, "sessions")
	s, err := NewStore(pool)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStore_LoadMissingReturnsZero(t *testing.T) {
	s := setupSessionStore(t)
	summary, count, err := s.Load(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if summary != "" || count != 0 {
		t.Errorf("missing row: got (%q, %d), want empty", summary, count)
	}
}

func TestStore_LoadEmptyKeyIsZeroNoError(t *testing.T) {
	s := setupSessionStore(t)
	summary, count, err := s.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load(empty): %v", err)
	}
	if summary != "" || count != 0 {
		t.Errorf("empty key: got (%q, %d), want empty", summary, count)
	}
}

func TestStore_SaveThenLoad(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("slack:C123", "U456")
	if err := s.Save(ctx, key, "running summary v1", 4, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	summary, count, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if summary != "running summary v1" {
		t.Errorf("summary = %q, want %q", summary, "running summary v1")
	}
	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}
}

func TestStore_SaveUpsertsOnConflict(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("slack:C123", "U456")
	if err := s.Save(ctx, key, "v1", 2, ""); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if err := s.Save(ctx, key, "v2", 7, ""); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	summary, count, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if summary != "v2" || count != 7 {
		t.Errorf("after upsert: got (%q, %d), want (%q, %d)", summary, count, "v2", 7)
	}
}

func TestStore_SaveEmptyKeyErrors(t *testing.T) {
	s := setupSessionStore(t)
	if err := s.Save(context.Background(), "", "x", 1, ""); err == nil {
		t.Error("expected error for empty key")
	}
}

func TestStore_DistinctKeysIndependent(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	a := Key("slack:C1", "U1")
	b := Key("slack:C2", "U2")
	if err := s.Save(ctx, a, "alpha", 3, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, b, "beta", 5, ""); err != nil {
		t.Fatal(err)
	}

	sumA, cntA, err := s.Load(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	sumB, cntB, err := s.Load(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if sumA != "alpha" || cntA != 3 {
		t.Errorf("A: got (%q, %d)", sumA, cntA)
	}
	if sumB != "beta" || cntB != 5 {
		t.Errorf("B: got (%q, %d)", sumB, cntB)
	}
}

func TestKeyFormat(t *testing.T) {
	if got := Key("slack:C1", "U2"); got != "slack:C1|U2" {
		t.Errorf("Key = %q, want %q", got, "slack:C1|U2")
	}
}

// scopeTagOf reads the scope_tag column directly. Load() doesn't expose
// the column (current callers don't need it), so the test peeks at the
// row to verify the persistence layer.
func scopeTagOf(t *testing.T, s *Store, key string) (string, bool) {
	t.Helper()
	var st sqlNullable
	err := s.db.QueryRowContext(context.Background(),
		`SELECT scope_tag FROM sessions WHERE session_key = $1`, key,
	).Scan(&st)
	if err != nil {
		t.Fatalf("scope_tag query: %v", err)
	}
	return st.Value, st.Valid
}

// sqlNullable is a thin wrapper to avoid importing database/sql here
// just for a single sql.NullString. The store_test file otherwise
// stays driver-agnostic.
type sqlNullable struct {
	Value string
	Valid bool
}

func (n *sqlNullable) Scan(src any) error {
	if src == nil {
		n.Valid = false
		n.Value = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		n.Value = v
	case []byte:
		n.Value = string(v)
	}
	n.Valid = true
	return nil
}

func TestStore_SaveTagsRowWithScope(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("shards:cannonball", "user@example.com")
	if err := s.Save(ctx, key, "shard summary", 1, "shard:cannonball-charger"); err != nil {
		t.Fatalf("Save with scope: %v", err)
	}
	got, valid := scopeTagOf(t, s, key)
	if !valid {
		t.Fatalf("expected non-NULL scope_tag")
	}
	if got != "shard:cannonball-charger" {
		t.Errorf("scope_tag = %q, want shard:cannonball-charger", got)
	}
}

func TestStore_SaveEmptyScopeIsNull(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("cli", "trusted-user")
	if err := s.Save(ctx, key, "trusted summary", 1, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, valid := scopeTagOf(t, s, key)
	if valid {
		t.Errorf("trusted-path scope_tag should be NULL, got non-null")
	}
}

// TestStore_SaveCoalescesEmptyScope documents the preserve-on-empty
// behavior of the upsert: once a session is bound to a shard, a
// subsequent unscoped save (defensive against a Step-7 key collision)
// keeps the existing tag rather than unbinding the session.
func TestStore_SaveCoalescesEmptyScope(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("shards:notes", "user@example.com")
	if err := s.Save(ctx, key, "v1", 1, "shard:notes"); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if err := s.Save(ctx, key, "v2", 2, ""); err != nil {
		t.Fatalf("Save v2 (no scope): %v", err)
	}
	got, valid := scopeTagOf(t, s, key)
	if !valid || got != "shard:notes" {
		t.Errorf("scope_tag after empty re-save = (%q, valid=%v); want (%q, true)", got, valid, "shard:notes")
	}
	// Confirm the summary did update — only scope_tag should be
	// preserved by the COALESCE.
	summary, count, err := s.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "v2" || count != 2 {
		t.Errorf("Load after re-save = (%q, %d); want (v2, 2)", summary, count)
	}
}

// TestStore_SaveRebindsToNewScope documents the converse: a Save with a
// non-empty scope_tag DOES overwrite an existing tag. This makes
// re-binding within the same shard idempotent (same value re-saved =
// no change) and also lets a future migration repoint a session to a
// different shard explicitly.
func TestStore_SaveRebindsToNewScope(t *testing.T) {
	s := setupSessionStore(t)
	ctx := context.Background()
	key := Key("shards:rebind", "user@example.com")
	if err := s.Save(ctx, key, "v1", 1, "shard:old"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, key, "v2", 2, "shard:new"); err != nil {
		t.Fatal(err)
	}
	got, _ := scopeTagOf(t, s, key)
	if got != "shard:new" {
		t.Errorf("scope_tag after rebind = %q, want shard:new", got)
	}
}
