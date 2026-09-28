package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/familiar/gateway/internal/push"
	"github.com/familiar/gateway/internal/testutil"
)

// Subscribing refuses an endpoint that isn't a push service: the
// gateway POSTs to it on every notification, and it used to accept any
// URL (the loopback model server's admin API, say).
func TestPushSubscribe_RefusesNonPushEndpoints(t *testing.T) {
	pool := testutil.PgTestPool(t)
	h := &Handler{}
	h.AttachPush(push.NewStore(pool), "vapid-public", push.EndpointPolicy{})
	sub := func(endpoint string) int {
		body := `{"endpoint":"` + endpoint + `","keys":{"p256dh":"k","auth":"a"}}`
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), ctxAuthUserKey, AuthUser{UserID: "push-policy-user"}))
		w := httptest.NewRecorder()
		h.pushSubscribe(w, r)
		return w.Code
	}
	for _, ep := range []string{"http://127.0.0.1:8081/slots/0?action=erase", "https://10.0.0.5/x", "https://fcm.googleapis.com.evil.example/x"} {
		if code := sub(ep); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", ep, code)
		}
	}
}
