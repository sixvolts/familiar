package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
)

// Fact identity and version history (DB-gated via setupMemoryStore).

// A row's supersedes pointer and whether any row points at it.
func chainState(t *testing.T, s *PgVectorStore, id string) (supersedes string, hidden, exists bool) {
	t.Helper()
	row, err := s.GetMemory(context.Background(), id)
	if errors.Is(err, ErrMemoryNotFound) {
		return "", false, false
	}
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return row.Supersedes, row.Superseded, true
}

// Deleting the newest version of a fact deletes the versions it
// replaced: otherwise nothing hides the older one any more and it comes
// back as current ("forget where I live" resurrecting "I live in
// Boston").
func TestDeleteMemory_ChainHeadTakesOlderVersions(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	boston := insertMemory(t, s, "I live in Boston", "user", axisVec(1))
	chicago := insertMemory(t, s, "I live in Chicago", "user", axisVec(1))
	nyc := insertMemory(t, s, "I live in NYC", "user", axisVec(1))
	setSupersedes(t, s, chicago, boston)
	setSupersedes(t, s, nyc, chicago)
	// A second row the sleep dedup also pointed at the oldest version.
	sibling := insertMemory(t, s, "I have lived in Boston", "user", axisVec(1))
	setSupersedes(t, s, sibling, boston)
	unrelated := insertMemory(t, s, "I like tea", "user", axisVec(2))

	if err := s.DeleteMemory(ctx, nyc); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	for _, id := range []string{nyc, chicago, boston} {
		if _, _, exists := chainState(t, s, id); exists {
			t.Errorf("version %s survived deleting the chain's head; nothing hides it now", id)
		}
	}
	if sup, _, exists := chainState(t, s, sibling); !exists || sup != "" {
		t.Errorf("a row outside the chain that pointed into it: exists=%v supersedes=%q, want kept and detached", exists, sup)
	}
	if _, _, exists := chainState(t, s, unrelated); !exists {
		t.Error("an unrelated memory was deleted")
	}

	// Deleting a middle version takes the older ones and leaves the
	// newer one live.
	v1 := insertMemory(t, s, "tea v1", "user", axisVec(3))
	v2 := insertMemory(t, s, "tea v2", "user", axisVec(3))
	v3 := insertMemory(t, s, "tea v3", "user", axisVec(3))
	setSupersedes(t, s, v2, v1)
	setSupersedes(t, s, v3, v2)
	if err := s.DeleteMemory(ctx, v2); err != nil {
		t.Fatalf("DeleteMemory (middle): %v", err)
	}
	if _, _, exists := chainState(t, s, v1); exists {
		t.Error("deleting a middle version left the version it replaced, which is now live")
	}
	if sup, hidden, exists := chainState(t, s, v3); !exists || hidden || sup != "" {
		t.Errorf("newer version after a middle delete: exists=%v hidden=%v supersedes=%q", exists, hidden, sup)
	}
}

// forget_fact's owner-scoped delete does the same, and a non-owner
// still changes nothing.
func TestDeleteMemoryOwned_ChainHeadTakesOlderVersions(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	old := insertFor(t, s, "u1", "I live in Boston", "", axisVec(1))
	cur := insertFor(t, s, "u1", "I live in NYC", "", axisVec(1))
	setSupersedes(t, s, cur, old)

	if ok, err := s.DeleteMemoryOwned(ctx, cur, "u2"); err != nil || ok {
		t.Fatalf("non-owner delete: ok=%v err=%v", ok, err)
	}
	if sup, _, exists := chainState(t, s, cur); !exists || sup != old {
		t.Fatal("a non-owner's delete changed the chain")
	}
	if ok, err := s.DeleteMemoryOwned(ctx, cur, "u1"); err != nil || !ok {
		t.Fatalf("owner delete: ok=%v err=%v", ok, err)
	}
	if _, _, exists := chainState(t, s, old); exists {
		t.Error("forgetting the current version brought back the old one")
	}
}

// A shard forgetting its own fact must not take down an owner's
// top-level fact that the shard's row superseded: the walk through older
// versions stays in the forgotten row's scope.
func TestDeleteMemoryOwned_ChainStaysInScope(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	owner := insertFor(t, s, "u1", "The dog is Rex", "", axisVec(1))
	shardOld := insertFor(t, s, "u1", "The dog was Spot", "shard:kiosk", axisVec(1))
	shardHead := insertFor(t, s, "u1", "The dog is Max", "shard:kiosk", axisVec(1))
	setSupersedes(t, s, shardOld, owner)
	setSupersedes(t, s, shardHead, shardOld)

	if ok, err := s.DeleteMemoryOwned(ctx, shardHead, "u1"); err != nil || !ok {
		t.Fatalf("DeleteMemoryOwned: ok=%v err=%v", ok, err)
	}
	if _, _, exists := chainState(t, s, shardOld); exists {
		t.Error("the shard's own older version survived")
	}
	if _, hidden, exists := chainState(t, s, owner); !exists || hidden {
		t.Errorf("owner's top-level fact: exists=%v hidden=%v; a shard's forget reached outside its scope", exists, hidden)
	}
}

// An edit moves the row's dedup hash with its content, and refuses to
// give it the identity of another row.
func TestUpdateMemoryContent_MovesHash(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	id := insertFor(t, s, "u1", "the cat is Tom", "shard:pets", axisVec(1))
	if err := s.UpdateMemoryContent(ctx, id, "the cat is Felix", "user:u1", axisVec(1)); err != nil {
		t.Fatalf("UpdateMemoryContent: %v", err)
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM memories WHERE id = $1::uuid`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if want := FactHash("u1", "shard:pets", "explicit", "", "the cat is Felix"); hash != want {
		t.Errorf("content_hash after edit = %q, want the new content's %q", hash, want)
	}

	other := insertFor(t, s, "u1", "the dog is Rex", "shard:pets", axisVec(2))
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET content_hash = $2 WHERE id = $1::uuid`,
		other, FactHash("u1", "shard:pets", "explicit", "", "the dog is Rex")); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateMemoryContent(ctx, id, "the dog is Rex", "user:u1", axisVec(2))
	if !errors.Is(err, ErrDuplicateContent) {
		t.Errorf("editing a row into another row's exact content: err = %v, want ErrDuplicateContent", err)
	}
}

// The migration moves wiki facts from slug refs to page ids, drops the
// ones no live page owns, and rehashes scoped rows with the same
// expression FactHash computes.
func TestMigrate_WikiFactsKeyedByPageID(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "wiki-mig-" + suffix
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, display_name, status) VALUES ($1, 'W', 'approved')`, user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var book string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO books (slug, name, created_by) VALUES ($1, 'Notes', $2) RETURNING id::text`,
		"notes-"+suffix, user).Scan(&book); err != nil {
		t.Fatalf("seed book: %v", err)
	}
	page := func(slug string, deleted bool) string {
		var id string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO wiki_pages (book_id, slug, title, created_by, updated_by, deleted_at)
			VALUES ($1::uuid, $2, $2, $3, $3, CASE WHEN $4 THEN NOW() END) RETURNING id::text`,
			book, slug, user, deleted).Scan(&id); err != nil {
			t.Fatalf("seed page %s: %v", slug, err)
		}
		return id
	}
	biopsy := page("biopsy", false)
	page("old-page", true)
	scope := "book:" + book
	legacy := func(content, scopeTag, sourceType, ref string) string {
		var sc, sr any
		if scopeTag != "" {
			sc = scopeTag
		}
		if ref != "" {
			sr = ref
		}
		sum := sha256.Sum256([]byte(user + "\x00" + content))
		var id string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO memories (agent_id, scope, content, content_hash, source_type, source_ref, user_id, scope_tag)
			VALUES ('test', 'user', $1, $2, $3, $4, $5, $6) RETURNING id::text`,
			content, hex.EncodeToString(sum[:]), sourceType, sr, user, sc).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", content, err)
		}
		return id
	}
	live := legacy("The biopsy is on Monday.", scope, "wiki_page", "notes-"+suffix+"/biopsy")
	renamed := legacy("Draft note from before the rename.", scope, "wiki_page", "notes-"+suffix+"/untitled")
	gone := legacy("Fact from a deleted page.", scope, "wiki_page", "notes-"+suffix+"/old-page")
	shard := legacy("The cat is Tom.", "shard:pets", "conversation_extraction", "sess-1")
	top := legacy("I like tea.", "", "conversation_extraction", "sess-1")
	var topHash string
	if err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM memories WHERE id = $1::uuid`, top).Scan(&topHash); err != nil {
		t.Fatal(err)
	}

	// A gated one-shot that setupMemoryStore's Migrate already ran.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM applied_data_fixes WHERE name = 'wiki_page_fact_identity'`); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := db.Migrate(ctx, s.db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	row, err := s.GetMemory(ctx, live)
	if err != nil {
		t.Fatalf("the live page's fact is gone: %v", err)
	}
	if row.SourceRef != "page:"+biopsy {
		t.Errorf("live page's fact source_ref = %q, want page:%s", row.SourceRef, biopsy)
	}
	for name, id := range map[string]string{"renamed page's": renamed, "deleted page's": gone} {
		if _, _, exists := chainState(t, s, id); exists {
			t.Errorf("the %s orphaned fact survived", name)
		}
	}
	hashOf := func(id string) string {
		var h string
		if err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM memories WHERE id = $1::uuid`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	// The migration's SQL hash and FactHash must agree.
	if got, want := hashOf(live), FactHash(user, scope, "wiki_page", "page:"+biopsy, "The biopsy is on Monday."); got != want {
		t.Errorf("wiki row hash = %s, want FactHash's %s", got, want)
	}
	if got, want := hashOf(shard), FactHash(user, "shard:pets", "conversation_extraction", "sess-1", "The cat is Tom."); got != want {
		t.Errorf("shard row hash = %s, want FactHash's %s", got, want)
	}
	if got := hashOf(top); got != topHash {
		t.Error("an unscoped row's hash changed; it would stop deduping")
	}
}
