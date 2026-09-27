package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/actions"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/testutil"
)

func actionsHandler(t *testing.T) (*Handler, *actions.Store, string) {
	t.Helper()
	pool := testutil.PgScopedPool(t, "admin_actions_review")
	user := fmt.Sprintf("act-%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO users (id, display_name, status, role) VALUES ($1, $1, 'approved', 'user') ON CONFLICT (id) DO NOTHING`, user); err != nil {
		t.Fatal(err)
	}
	store, err := actions.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := actions.NewRunner(actions.Deps{
		Store: store, Sessions: session.NewManager(),
		Invoke: func(context.Context, *session.Session, string, *pipeline.ShardOverrides) (string, *pipeline.RouteInfo, error) {
			return "", nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.AttachActions(store, runner)
	return h, store, user
}

func actionReq(h *Handler, method, user, id, body string, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/console/api/actions", strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	req = req.WithContext(ctxWithAuth(req.Context(), AuthUser{UserID: user, Role: "user"}))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

const actionJSON = `{"name":"digest","prompt":"summarize","trigger_kind":"cron","cron":"0 7 * * *","report_targets":[{"kind":"log"}]%s}`

// min_interval_seconds 0 means no throttle and is kept; unset defaults
// to 60. 0 was coerced to 60.
func TestCreateAction_ExplicitZeroInterval(t *testing.T) {
	h, _, user := actionsHandler(t)
	for extra, want := range map[string]int{`,"min_interval_seconds":0`: 0, ``: 60} {
		rec := actionReq(h, "POST", user, "", fmt.Sprintf(actionJSON, extra), h.createAction)
		var a actions.Action
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
			t.Fatalf("create %q = %d %s", extra, rec.Code, rec.Body.String())
		}
		if a.MinIntervalSeconds != want {
			t.Errorf("create %q: min_interval_seconds = %d, want %d", extra, a.MinIntervalSeconds, want)
		}
	}
}

// Widening an action to "run as you" takes an explicit confirm; the
// panel fell back to "user" when it couldn't list the stored shard and
// re-enveloped the action on an unrelated save.
func TestPatchAction_WideningNeedsConfirm(t *testing.T) {
	h, _, user := actionsHandler(t)
	rec := actionReq(h, "POST", user, "", fmt.Sprintf(actionJSON, `,"envelope":"ephemeral"`), h.createAction)
	var a actions.Action
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{"envelope":"user"}`, `{"envelope":""}`} {
		if rec := actionReq(h, "PATCH", user, a.ID, body, h.patchAction); rec.Code != http.StatusBadRequest {
			t.Errorf("unconfirmed widening %s = %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	if rec := actionReq(h, "PATCH", user, a.ID, `{"name":"renamed"}`, h.patchAction); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"envelope":"ephemeral"`) {
		t.Errorf("unrelated edit = %d %s, want the envelope kept", rec.Code, rec.Body.String())
	}
	if rec := actionReq(h, "PATCH", user, a.ID, `{"envelope":"user","confirm_envelope":true}`, h.patchAction); rec.Code != http.StatusOK {
		t.Errorf("confirmed widening = %d %s", rec.Code, rec.Body.String())
	}
}

// Enabling checks the row: a shard_deleted action (envelope shard, no
// shard) was re-enabled and failed every fire until the breaker tripped.
func TestEnableAction_RefusesAnInvalidRow(t *testing.T) {
	h, store, user := actionsHandler(t)
	rec := actionReq(h, "POST", user, "", fmt.Sprintf(actionJSON, ``), h.createAction)
	var a actions.Action
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	// The state DisableByShard leaves behind.
	pool := testutil.PgScopedPool(t, "admin_actions_review")
	if _, err := pool.ExecContext(context.Background(), `UPDATE scheduled_actions SET envelope = 'shard', shard_id = NULL, enabled = false, last_status = 'shard_deleted' WHERE id = $1::uuid`, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec := actionReq(h, "POST", user, a.ID, ``, h.setActionEnabled(true)); rec.Code != http.StatusBadRequest {
		t.Errorf("enable of a shard_deleted action = %d %s, want 400", rec.Code, rec.Body.String())
	}
	got, err := store.Get(context.Background(), a.ID, user, false)
	if err != nil || got.Enabled {
		t.Errorf("the action was enabled anyway (%v)", err)
	}
	if rec := actionReq(h, "POST", user, a.ID, ``, h.setActionEnabled(false)); rec.Code != http.StatusOK {
		t.Errorf("disable = %d", rec.Code)
	}
}
