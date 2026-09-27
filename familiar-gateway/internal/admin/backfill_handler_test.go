package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/backfill"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/sidecar"
)

type noopExtractor struct{}

func (noopExtractor) ExtractRelationshipsFromFacts(context.Context, []string) ([]sidecar.ExtractedRelationship, error) {
	return nil, nil
}

type noopSink struct{}

func (noopSink) InsertRelationshipsIfAbsent(context.Context, []memory.Relationship) (int, error) {
	return 0, nil
}

type noopSource struct{}

func (noopSource) ListForBackfill(context.Context, string) ([]backfill.Item, error) { return nil, nil }

// A backfill needs a user: without one it scanned nothing and reported
// a successful run.
func TestStartBackfill_RequiresUser(t *testing.T) {
	h := &Handler{}
	h.backfillDeps = &backfill.Deps{Source: noopSource{}, Extractor: noopExtractor{}, Sink: noopSink{}}
	for body, want := range map[string]int{`{}`: http.StatusBadRequest, `{"user_id":"  "}`: http.StatusBadRequest} {
		rec := httptest.NewRecorder()
		h.startBackfill(rec, httptest.NewRequest("POST", "/admin/api/controls/backfill-relationships", strings.NewReader(body)))
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", body, rec.Code, want)
		}
	}
	if h.backfillState != nil {
		t.Error("a run started without a user")
	}
}
