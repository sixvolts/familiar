package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Page writes take a bounded body; there was no limit at all.
func TestDecodePageBody_Caps(t *testing.T) {
	var body struct {
		Content string `json:"content"`
	}
	big := `{"content":"` + strings.Repeat("x", maxPageBodyBytes) + `"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(big))
	if decodePageBody(w, r, &body) || w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an over-cap page body: code %d, want 413", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(`{"content":"`+strings.Repeat("x", 100<<10)+`"}`))
	if !decodePageBody(w, r, &body) || len(body.Content) != 100<<10 {
		t.Errorf("a 100KB page was refused: code %d", w.Code)
	}
}
