package memengine

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/memory"
	pb "github.com/familiar/gateway/proto/engine"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Fact identity, re-assertion and version history through the real
// engine (DB-gated; each test migrates a private schema).

const idUser = "user-identity"

// fact builds a top-level fact for idUser.
func fact(content string, supersedes string) *pb.FactProto {
	now := time.Now()
	return &pb.FactProto{
		Id:           uuid.NewString(),
		Content:      content,
		UserId:       idUser,
		Scope:        "user",
		SourceType:   "conversation_extraction",
		Supersedes:   supersedes,
		CreatedAt:    timestamppb.New(now),
		LastAccessed: timestamppb.New(now),
	}
}

// commit stores f and returns the id of the row that holds it.
func commit(t *testing.T, e *MemEngine, f *pb.FactProto) string {
	t.Helper()
	if _, err := e.CommitFacts(context.Background(), "sess-identity", []*pb.FactProto{f}); err != nil {
		t.Fatalf("CommitFacts %q: %v", f.Content, err)
	}
	return f.Id
}

type rowState struct {
	exists, hidden bool
	supersedes     string
	scopeTag       string
}

func stateOf(t *testing.T, e *MemEngine, id string) rowState {
	t.Helper()
	var sup, tag sql.NullString
	var st rowState
	err := e.pool.QueryRowContext(context.Background(), `
		SELECT m.supersedes::text, m.scope_tag,
		       EXISTS (SELECT 1 FROM memories s WHERE s.supersedes = m.id)
		  FROM memories m WHERE m.id = $1::uuid`, id).Scan(&sup, &tag, &st.hidden)
	if err == sql.ErrNoRows {
		return st
	}
	if err != nil {
		t.Fatalf("state %s: %v", id, err)
	}
	st.exists, st.supersedes, st.scopeTag = true, sup.String, tag.String
	return st
}

// Portland, then Seattle, then back to Portland: the restatement lands
// on the old Portland row, which has to become the live fact again,
// with Seattle superseded. The upsert used to just bump Portland's
// counters, leaving it hidden and Seattle live for good.
func TestCommitFacts_ReassertedFactComesBack(t *testing.T) {
	e := setupReembedTest(t)
	portland := commit(t, e, fact("User lives in Portland", ""))
	seattle := commit(t, e, fact("User lives in Seattle", portland))
	if !stateOf(t, e, portland).hidden {
		t.Fatal("setup: Seattle should hide Portland")
	}

	again := fact("User lives in Portland", seattle)
	fresh := again.Id
	got := commit(t, e, again)
	if got != portland {
		t.Errorf("restated fact reported id %s (fresh %s), want the row that holds it, %s", got, fresh, portland)
	}
	if p := stateOf(t, e, portland); p.hidden || p.supersedes != seattle {
		t.Errorf("Portland after the restatement: hidden=%v supersedes=%q, want live and superseding Seattle", p.hidden, p.supersedes)
	}
	if s := stateOf(t, e, seattle); !s.hidden || s.supersedes != "" {
		t.Errorf("Seattle after the restatement: hidden=%v supersedes=%q, want hidden with no pointer back to Portland", s.hidden, s.supersedes)
	}
}

// A plain restatement (no supersedes) still brings a hidden fact back.
func TestCommitFacts_RestatementUnhides(t *testing.T) {
	e := setupReembedTest(t)
	blue := commit(t, e, fact("User's favorite color is blue", ""))
	commit(t, e, fact("User's favorite color is green", blue))
	commit(t, e, fact("User's favorite color is blue", ""))
	if stateOf(t, e, blue).hidden {
		t.Error("the user restated a fact and it stayed hidden")
	}

	// A restatement naming its own row as the one it replaces must not
	// hide itself.
	commit(t, e, fact("User's favorite color is blue", blue))
	if s := stateOf(t, e, blue); s.hidden || s.supersedes == blue {
		t.Errorf("a fact restated as replacing itself: hidden=%v supersedes=%q", s.hidden, s.supersedes)
	}
}

// Moving a row back to the head keeps the history it hid hidden.
func TestCommitFacts_ReassertKeepsOlderHistoryHidden(t *testing.T) {
	e := setupReembedTest(t)
	// oldest <- x <- head: x is re-stated as replacing head.
	oldest := commit(t, e, fact("Office is on floor 1", ""))
	x := commit(t, e, fact("Office is on floor 2", oldest))
	head := commit(t, e, fact("Office is on floor 3", x))
	commit(t, e, fact("Office is on floor 2", head))
	if s := stateOf(t, e, x); s.hidden || s.supersedes != head {
		t.Errorf("x: hidden=%v supersedes=%q, want live superseding head", s.hidden, s.supersedes)
	}
	if s := stateOf(t, e, head); !s.hidden || s.supersedes != oldest {
		t.Errorf("head: hidden=%v supersedes=%q, want hidden and holding x's old predecessor", s.hidden, s.supersedes)
	}
	if !stateOf(t, e, oldest).hidden {
		t.Error("the oldest version came back")
	}

	// x is live with a predecessor and nothing above it, and is
	// re-stated as replacing another live fact t: t takes over hiding
	// x's predecessor (x -> t -> prev).
	prev := commit(t, e, fact("Car is red", ""))
	x2 := commit(t, e, fact("Car is blue", prev))
	tgt := commit(t, e, fact("Car is black", ""))
	commit(t, e, fact("Car is blue", tgt))
	if s := stateOf(t, e, x2); s.hidden || s.supersedes != tgt {
		t.Errorf("x2: hidden=%v supersedes=%q, want live superseding the target", s.hidden, s.supersedes)
	}
	if s := stateOf(t, e, tgt); !s.hidden || s.supersedes != prev {
		t.Errorf("target: hidden=%v supersedes=%q, want hidden, holding x2's predecessor", s.hidden, s.supersedes)
	}
	if !stateOf(t, e, prev).hidden {
		t.Error("x2's predecessor came back")
	}

	// A target already older than x in its own chain changes nothing.
	commit(t, e, fact("Car is blue", prev))
	if s := stateOf(t, e, x2); s.supersedes != tgt {
		t.Errorf("x2 re-pointed at its own ancestor: supersedes=%q", s.supersedes)
	}
}

// Identical content in another scope is a different fact. The upsert
// used to merge a shard's save onto the owner's row and COALESCE the
// shard's tag onto it, hiding the owner's fact from top-level memory.
func TestCommitFacts_ScopesDontMerge(t *testing.T) {
	e := setupReembedTest(t)
	top := commit(t, e, fact("The dog is Rex", ""))
	sh := fact("The dog is Rex", "")
	sh.ScopeTag = "shard:vet"
	shardRow := commit(t, e, sh)
	if shardRow == top {
		t.Fatal("a shard's fact merged onto the owner's top-level row")
	}
	if s := stateOf(t, e, top); s.scopeTag != "" {
		t.Errorf("the owner's top-level fact was retagged %q", s.scopeTag)
	}

	// Two wiki pages holding the same sentence each own a row, so
	// replacing one page's facts can't delete the other's.
	pageFact := func() *pb.FactProto {
		f := fact("Deploys go to staging first.", "")
		f.ScopeTag = "book:b1"
		return f
	}
	ctx := context.Background()
	if _, err := e.ReplaceSourceFacts(ctx, "wiki:b1", "wiki_page", "page:a", "book:b1", []*pb.FactProto{pageFact()}); err != nil {
		t.Fatal(err)
	}
	b := pageFact()
	if _, err := e.ReplaceSourceFacts(ctx, "wiki:b1", "wiki_page", "page:b", "book:b1", []*pb.FactProto{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReplaceSourceFacts(ctx, "wiki:b1", "wiki_page", "page:a", "book:b1", nil); err != nil {
		t.Fatal(err)
	}
	if !stateOf(t, e, b.Id).exists {
		t.Error("clearing page a deleted page b's copy of the same fact")
	}
}

// A legacy row whose hash predates the scope being part of it must not
// absorb a commit from another scope.
func TestCommitFacts_RefusesLegacyCrossScopeRow(t *testing.T) {
	e := setupReembedTest(t)
	ctx := context.Background()
	legacyID := uuid.NewString()
	if _, err := e.pool.ExecContext(ctx, `
		INSERT INTO memories (id, agent_id, scope, content, content_hash, source_type, user_id, scope_tag)
		VALUES ($1::uuid, $2, 'user', 'The cat is Tom', $3, 'conversation_extraction', $4, 'shard:pets')`,
		legacyID, e.agentID, factHash(idUser, "", "", "", "The cat is Tom"), idUser); err != nil {
		t.Fatal(err)
	}
	_, err := e.CommitFacts(ctx, "sess", []*pb.FactProto{fact("The cat is Tom", "")})
	if err == nil {
		t.Error("a top-level commit landed on a shard's legacy row")
	}
	if s := stateOf(t, e, legacyID); s.scopeTag != "shard:pets" {
		t.Errorf("legacy row's scope changed to %q", s.scopeTag)
	}
}

// ReplaceSourceFacts swaps a page's facts in one transaction: a failure
// leaves the old set in place.
func TestReplaceSourceFacts_Atomic(t *testing.T) {
	e := setupReembedTest(t)
	ctx := context.Background()
	oldFact := fact("The biopsy is on Monday", "")
	if _, err := e.ReplaceSourceFacts(ctx, "wiki:b", "wiki_page", "page:p", "book:b", []*pb.FactProto{oldFact}); err != nil {
		t.Fatal(err)
	}
	bad := fact("The biopsy moved to Tuesday", "not-a-uuid")
	if _, err := e.ReplaceSourceFacts(ctx, "wiki:b", "wiki_page", "page:p", "book:b",
		[]*pb.FactProto{fact("The biopsy is at 9am", ""), bad}); err == nil {
		t.Fatal("a replace with a bad fact reported success")
	}
	if !stateOf(t, e, oldFact.Id).exists {
		t.Error("a failed replace deleted the page's existing facts")
	}
	n, err := e.ReplaceSourceFacts(ctx, "wiki:b", "wiki_page", "page:p", "book:b",
		[]*pb.FactProto{fact("The biopsy moved to Tuesday", "")})
	if err != nil || n != 1 {
		t.Fatalf("replace: removed=%d err=%v, want 1 removed", n, err)
	}
	if stateOf(t, e, oldFact.Id).exists {
		t.Error("the replaced fact survived")
	}
}

// The engine's delete takes the older versions too, so forgetting the
// current fact can't resurrect the one it replaced.
func TestDeleteFact_RemovesOlderVersions(t *testing.T) {
	e := setupReembedTest(t)
	boston := commit(t, e, fact("I live in Boston", ""))
	nyc := commit(t, e, fact("I live in NYC", boston))
	resp, err := e.DeleteFact(context.Background(), "", nyc, &pb.VisibilityContext{UserId: idUser})
	if err != nil || !resp.Deleted {
		t.Fatalf("DeleteFact: %+v %v", resp, err)
	}
	if stateOf(t, e, boston).exists {
		t.Error("forgetting where the user lives brought back the old address")
	}
}

// correct_fact's engine path records what it overwrote.
func TestUpdateFact_RecordsVersions(t *testing.T) {
	e := setupReembedTest(t)
	ctx := context.Background()
	f := fact("Mom's birthday is May 5", "")
	f.ScopeTag = "shard:family"
	id := commit(t, e, f)
	resp, err := e.UpdateFact(ctx, "", id, "Mom's birthday is May 6", nil, &pb.VisibilityContext{UserId: idUser})
	if err != nil || !resp.Updated {
		t.Fatalf("UpdateFact: %+v %v", resp, err)
	}
	rows, err := e.pool.QueryContext(ctx,
		`SELECT content, change_type, COALESCE(changed_by, '') FROM memory_versions WHERE memory_id = $1::uuid ORDER BY version`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c, ct, by string
		if err := rows.Scan(&c, &ct, &by); err != nil {
			t.Fatal(err)
		}
		got = append(got, ct+":"+c+":"+by)
	}
	want := []string{"created:Mom's birthday is May 5:", "updated:Mom's birthday is May 6:user:" + idUser}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("versions = %q, want %q (the overwritten text must be recoverable)", got, want)
	}
	var hash string
	if err := e.pool.QueryRowContext(ctx, `SELECT content_hash FROM memories WHERE id = $1::uuid`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if want := memory.FactHash(idUser, "shard:family", "conversation_extraction", "", "Mom's birthday is May 6"); hash != want {
		t.Error("the edited row's hash ignores its scope; a restatement in the shard wouldn't dedup onto it")
	}
}
