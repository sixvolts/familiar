package admin

// Web Push subscription endpoints. A logged-in user registers a device's
// push subscription (so scheduled actions can notify it) and can remove
// it. The VAPID public key is handed out so the browser can subscribe.
// These manage the CALLER's own subscriptions only — no admin override.

import (
	"encoding/json"
	"net/http"

	"github.com/familiar/gateway/internal/push"
)

// AttachPush wires the push subscription store + the VAPID public key
// onto the handler. Idempotent; mirrors AttachConversationStore.
func (h *Handler) AttachPush(store *push.Store, vapidPublicKey string) {
	h.push = store
	h.pushVAPIDPublicKey = vapidPublicKey
}

func (h *Handler) ensurePush(w http.ResponseWriter) bool {
	if h.push == nil || h.pushVAPIDPublicKey == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "push notifications not configured on this deploy")
		return false
	}
	return true
}

// pushKey serves GET /console/api/push/key — the VAPID public key the
// browser needs to call pushManager.subscribe. Not secret.
func (h *Handler) pushKey(w http.ResponseWriter, r *http.Request) {
	if !h.ensurePush(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": h.pushVAPIDPublicKey})
}

// pushSubscribe serves POST /console/api/push/subscribe. Body is the
// browser's PushSubscription JSON: {endpoint, keys:{p256dh, auth}}.
func (h *Handler) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !h.ensurePush(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeJSONError(w, http.StatusBadRequest, "endpoint and keys (p256dh, auth) are required")
		return
	}
	if err := h.push.Upsert(r.Context(), au.UserID, push.Subscription{
		Endpoint: body.Endpoint,
		P256dh:   body.Keys.P256dh,
		Auth:     body.Keys.Auth,
	}, r.UserAgent()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "subscribed"})
}

// pushUnsubscribe serves DELETE /console/api/push/subscribe. Body:
// {endpoint}. Scoped to the caller's own subscriptions.
func (h *Handler) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !h.ensurePush(w) {
		return
	}
	au, ok := AuthUserFrom(r.Context())
	if !ok || au.UserID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing authenticated user")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.push.DeleteByEndpoint(r.Context(), au.UserID, body.Endpoint); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unsubscribed"})
}
