package pipeline

import (
	"encoding/json"
	"testing"
)

// researchNoteFrom gates the inline-path auto-open/link on the research
// convention: a create_page/update_page landing in the personal book
// with a "Research:" title. It must be tolerant of everything else.
func TestResearchNoteFrom(t *testing.T) {
	loc := func(book, page, title string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"book_slug": book, "page_slug": page, "title": title})
		return b
	}

	cases := []struct {
		name     string
		tool     string
		data     json.RawMessage
		wantOK   bool
		wantBook string
	}{
		{"create research note", "create_page", loc("personal:operator", "research-optane", "Research: Optane"), true, "personal:operator"},
		{"update research note", "update_page", loc("personal:operator", "research-optane", "Research: Optane"), true, "personal:operator"},
		{"compose_research_note", "compose_research_note", loc("personal:operator", "research-optane", "Research: Optane"), true, "personal:operator"},
		{"title with whitespace", "create_page", loc("personal:operator", "r", "  Research: X"), true, "personal:operator"},
		{"non-research title", "create_page", loc("personal:operator", "groceries", "Groceries"), false, ""},
		{"research title in shared book", "create_page", loc("engineering", "r", "Research: X"), false, ""},
		{"wrong tool", "append_to_page", loc("personal:operator", "r", "Research: X"), false, ""},
		{"empty page slug", "create_page", loc("personal:operator", "", "Research: X"), false, ""},
		{"absent data", "create_page", nil, false, ""},
		{"malformed data", "create_page", json.RawMessage("{not json"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := researchNoteFrom(tc.tool, tc.data)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ref=%+v)", ok, tc.wantOK, ref)
			}
			if ok && ref.BookSlug != tc.wantBook {
				t.Errorf("book = %q, want %q", ref.BookSlug, tc.wantBook)
			}
			if ok && ref.Title == "" {
				t.Error("ok but empty title")
			}
		})
	}
}
