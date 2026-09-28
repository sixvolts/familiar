package admin

import (
	"context"
	"testing"
)

// Link listings show only what the viewer can read. Resolution used to
// ignore the viewer, so linking [[personal:<someone>/journal]] from
// your own page told you whether it existed and its title, and a
// private note linking into a shared book showed its title to everyone
// in that book.
func TestPageLinks_OnlyResolveIntoReadableBooks(t *testing.T) {
	s, _ := wikiStoreForTest(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role) VALUES
		  ('link-victim', 'V', 'approved', 'user'), ('link-snoop', 'S', 'approved', 'user')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	victimBook, err := s.EnsurePersonalBook(ctx, "link-victim")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreatePage(ctx, victimBook.ID, "link-victim", "Divorce plan", "private", "divorce-plan")
	if err != nil {
		t.Fatal(err)
	}
	snoopBook, err := s.EnsurePersonalBook(ctx, "link-snoop")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := s.CreatePage(ctx, snoopBook.ID, "link-snoop", "Probe",
		"[["+victimBook.Slug+"/divorce-plan]]", "probe")
	if err != nil {
		t.Fatal(err)
	}

	links, err := s.ListPageLinks(ctx, probe.ID, LinkViewer{UserID: "link-snoop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %+v", links)
	}
	if links[0].TargetPageID != nil || links[0].TargetPageTitle != "" {
		t.Errorf("a link into an unreadable book resolved: id=%v title=%q", links[0].TargetPageID, links[0].TargetPageTitle)
	}
	// The owner (and an admin) see it resolved.
	for _, v := range []LinkViewer{{UserID: "link-victim"}, {UserID: "someone", IsAdmin: true}} {
		links, _ = s.ListPageLinks(ctx, probe.ID, v)
		if len(links) != 1 || links[0].TargetPageID == nil || *links[0].TargetPageID != secret.ID || links[0].TargetPageTitle != "Divorce plan" {
			t.Errorf("viewer %+v: links = %+v, want the resolved target", v, links)
		}
	}
	// A shard's book envelope narrows it further.
	links, _ = s.ListPageLinks(ctx, probe.ID, LinkViewer{UserID: "link-victim",
		CanAccessBook: func(id string) bool { return id != victimBook.ID }})
	if links[0].TargetPageID != nil {
		t.Error("a link resolved into a book outside the viewer's envelope")
	}

	// Backlinks: the victim's page links into a book the snoop can read;
	// the snoop must not see the victim's page in its backlinks.
	shared, err := s.CreateBook(ctx, "link-snoop", "Family", "", "")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := s.CreatePage(ctx, shared.ID, "link-snoop", "Budget", "numbers", "budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePage(ctx, victimBook.ID, "link-victim", "Leaving", "see [["+shared.Slug+"/budget]]", "leaving"); err != nil {
		t.Fatal(err)
	}
	back, err := s.ListBacklinks(ctx, budget.ID, LinkViewer{UserID: "link-snoop"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range back {
		if b.SourcePageTitle == "Leaving" {
			t.Error("a backlink from a private book the viewer can't read was listed")
		}
	}
	back, _ = s.ListBacklinks(ctx, budget.ID, LinkViewer{UserID: "link-victim"})
	if len(back) != 1 || back[0].SourcePageTitle != "Leaving" {
		t.Errorf("the source book's own member doesn't see the backlink: %+v", back)
	}
}
