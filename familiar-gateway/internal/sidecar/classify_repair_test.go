package sidecar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/familiar/gateway/internal/classifier"
	"github.com/familiar/gateway/internal/modelrole"
)

func classifyAgainst(t *testing.T, content string) (classifier.Output, ClassifyStats) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(map[string][]string{TaskClassify: {"fast"}},
		map[string]string{"fast": modelrole.StatusOnline}, map[string]string{"fast": srv.URL})
	return c.ClassifyWithStats(context.Background(), nil, "look up the latest Go release notes")
}

// "thinking":"none" is the prompt's own vocabulary slip. It used to fail
// Validate and turn the whole verdict into high/deep/none: web search
// vetoed, the condensed query dropped.
func TestClassify_NoneThinkingIsOff(t *testing.T) {
	out, st := classifyAgainst(t, `{"thinking":"none","memory_depth":"shallow","search_depth":"shallow","condensed_query":"go release notes"}`)
	if st.Err != nil || out.Source != classifier.SourceModel {
		t.Fatalf("a normalizable verdict was treated as a failure: err=%v source=%q", st.Err, out.Source)
	}
	if out.Thinking != classifier.ThinkingOff || out.SearchDepth != classifier.SearchShallow || out.CondensedQuery != "go release notes" {
		t.Errorf("verdict = %+v, want off/shallow with the condensed query", out)
	}
}

// A field that is really wrong is repaired alone; the rest is kept, and
// the verdict still counts as a fallback.
func TestClassify_BadFieldRepairedAlone(t *testing.T) {
	out, st := classifyAgainst(t, `{"thinking":"EXTREME","memory_depth":"shallow","search_depth":"shallow","condensed_query":"go release notes"}`)
	if st.Err == nil || out.Source != classifier.SourceUnparsed {
		t.Errorf("err=%v source=%q; a repaired verdict must stay measurable as a fallback", st.Err, out.Source)
	}
	if out.Thinking != classifier.ThinkingHigh || out.SearchDepth != classifier.SearchShallow ||
		out.MemoryDepth != classifier.MemoryShallow || out.CondensedQuery != "go release notes" {
		t.Errorf("verdict = %+v, want thinking high and the rest kept", out)
	}
}
