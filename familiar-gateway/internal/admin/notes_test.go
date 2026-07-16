package admin

// Notes-package tests — mostly retired alongside the notes HTTP
// surface (BOOKS-WIKI-ARCHITECTURE Phase 1 step 4 swapped to a
// facade, step 6 deleted the facade entirely). What remains:
//   - snippetFromContent: still used by WikiStore.ListPages, so
//     its preview behavior is worth pinning down.
//   - Note DTO round-trip: catches accidental JSON-tag drift on
//     the legacy struct (kept around because notes table is the
//     post-migration backup).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSnippetFromContent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello world", "hello world"},
		{"# Heading\nbody", "Heading"},
		{"> quoted line\nrest", "quoted line"},
		{"- list item\n- second", "list item"},
		{strings.Repeat("a", 200), strings.Repeat("a", snippetMaxRunes-1) + "…"},
	}
	for _, tc := range cases {
		got := snippetFromContent(tc.in)
		if got != tc.want {
			t.Errorf("snippetFromContent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNoteDTO_RoundTrip(t *testing.T) {
	deletedAt := time.Now().UTC().Truncate(time.Second)
	n := Note{
		ID: "n1", UserID: "operator", Title: "T", Content: "body",
		Folder: "Inbox", Pinned: true,
		CreatedAt: deletedAt, UpdatedAt: deletedAt,
		DeletedAt: &deletedAt,
	}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"id":"n1"`, `"user_id":"operator"`, `"folder":"Inbox"`,
		`"pinned":true`, `"deleted_at":`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("missing %q in %s", field, raw)
		}
	}
	var back Note
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.DeletedAt == nil {
		t.Error("deleted_at lost on round-trip")
	}
}
