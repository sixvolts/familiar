package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// seedBook creates a book with these members and returns its id.
func seedBook(t *testing.T, s *PgVectorStore, slug string, members ...string) string {
	t.Helper()
	ctx := context.Background()
	for _, u := range append([]string{"outsider"}, members...) {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO users (id, display_name, status, role) VALUES ($1, $1, 'approved', 'user')
			 ON CONFLICT (id) DO NOTHING`, u); err != nil {
			t.Fatalf("seed user %s: %v", u, err)
		}
	}
	var id string
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO books (slug, name, created_by) VALUES ($1, $1, $2) RETURNING id::text`,
		slug, members[0]).Scan(&id); err != nil {
		t.Fatalf("insert book: %v", err)
	}
	for _, u := range members {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO book_members (book_id, user_id, role) VALUES ($1::uuid, $2, 'writer')`, id, u); err != nil {
			t.Fatalf("add member %s: %v", u, err)
		}
	}
	t.Cleanup(func() { _, _ = s.db.ExecContext(context.Background(), `DELETE FROM books WHERE id = $1::uuid`, id) })
	return id
}

// A wiki page's facts and triples are recalled by every current member
// of its book, whoever saved it. They were keyed to the saver: only the
// member who last saved a page recalled it, and a member removed from
// the book still did.
func TestRecall_WikiFactsFollowBookMembership(t *testing.T) {
	s := setupMemoryStore(t)
	ctx := context.Background()
	book := seedBook(t, s, "recall-recipes", "saver", "spouse")
	// Membership of another book grants nothing here.
	seedBook(t, s, "recall-elsewhere", "outsider")
	tag := "book:" + book
	vec := []float32{0.9, 0.1, 0.1}
	insertFor(t, s, "saver", "The sourdough starter lives in the blue jar", tag, vec)

	rels, err := NewPgRelationshipStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	clearRels := func() {
		_, _ = s.db.ExecContext(context.Background(),
			`DELETE FROM relationships WHERE user_id IN ('saver', 'spouse', 'outsider')`)
	}
	clearRels()
	t.Cleanup(clearRels)
	if err := rels.UpsertRelationships(ctx, []Relationship{{
		Subject: "sourdough starter", Predicate: "lives_in", Object: "blue jar",
		UserID: "saver", ScopeTag: tag, Confidence: 0.9,
	}}); err != nil {
		t.Fatal(err)
	}

	sees := func(user string) map[string]bool {
		out := map[string]bool{}
		if res, err := s.Search(ctx, vec, 5, 0.5, user); err != nil {
			t.Fatalf("Search: %v", err)
		} else {
			out["search"] = len(res) > 0
		}
		if res, err := s.HybridSearch(ctx, "sourdough starter", vec, 5, 0.5, user); err != nil {
			t.Fatalf("HybridSearch: %v", err)
		} else {
			out["hybrid"] = len(res) > 0
		}
		if res, err := s.HybridSearch(ctx, "sourdough starter", nil, 5, 0.5, user); err != nil {
			t.Fatalf("keyword: %v", err)
		} else {
			out["keyword"] = len(res) > 0
		}
		if res, err := rels.RelatedForContents(ctx, []string{"where is the sourdough starter"}, user, 5); err != nil {
			t.Fatalf("RelatedForContents: %v", err)
		} else {
			out["related"] = len(res) > 0
		}
		if res, err := rels.TraverseFrom(ctx, "sourdough starter", user, 2, 5); err != nil {
			t.Fatalf("TraverseFrom: %v", err)
		} else {
			out["traverse"] = len(res) > 0
		}
		vocab := NewEntityVocab(rels, time.Hour)
		if err := vocab.Refresh(ctx, user); err != nil {
			t.Fatalf("vocab: %v", err)
		}
		out["vocab"] = len(vocab.FindIn(user, "where is the sourdough starter")) > 0
		return out
	}
	all := func(m map[string]bool, want bool) string {
		var bad []string
		for k, v := range m {
			if v != want {
				bad = append(bad, k)
			}
		}
		return strings.Join(bad, ",")
	}

	if bad := all(sees("saver"), true); bad != "" {
		t.Errorf("the saver doesn't recall it via %s", bad)
	}
	if bad := all(sees("spouse"), true); bad != "" {
		t.Errorf("the other member doesn't recall it via %s", bad)
	}
	if bad := all(sees("outsider"), false); bad != "" {
		t.Errorf("a non-member recalls it via %s", bad)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM book_members WHERE book_id = $1::uuid AND user_id = 'spouse'`, book); err != nil {
		t.Fatal(err)
	}
	if bad := all(sees("spouse"), false); bad != "" {
		t.Errorf("a removed member still recalls it via %s", bad)
	}
}
