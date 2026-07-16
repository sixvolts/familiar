package admin

import (
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// pendingStore holds in-flight webauthn.SessionData between a Begin
// call and the Finish that follows. Each pending entry is keyed by a
// random token handed back to the browser in a short-lived cookie and
// auto-evicted after pendingTTL so abandoned ceremonies can't
// accumulate.
type pendingStore struct {
	mu      sync.Mutex
	entries map[string]pendingEntry
}

type pendingEntry struct {
	data webauthn.SessionData
	// kind disambiguates which Begin handler minted this entry so a
	// Finish handler can refuse cross-flow completion. Without this
	// a single pending-cookie token could be replayed across
	// handlers — e.g. completing an enrollment-token ceremony
	// through registerFinish (which would NOT consume the token),
	// or a shard-passkey ceremony through registerFinish. Allowed
	// values are the PendingKind* constants below.
	kind    string
	userID  string // set during registration; empty during login (discoverable)
	expires time.Time
}

// PendingKind* tag pending ceremony entries by the Begin handler
// that minted them. Each Finish handler refuses entries whose kind
// doesn't match the flow it expects.
const (
	PendingKindRegister      = "register"       // /register/begin
	PendingKindLogin         = "login"          // /login/begin (discoverable credential)
	PendingKindEnroll        = "enroll"         // cross-domain /enroll/begin (token-driven)
	PendingKindShardRegister = "shard-register" // /shards/{id}/passkeys/begin
)

const pendingTTL = 5 * time.Minute

func newPendingStore() *pendingStore {
	return &pendingStore{entries: make(map[string]pendingEntry)}
}

// put records an in-flight ceremony. kind is one of the PendingKind*
// constants; Finish handlers MUST verify it matches their flow.
func (p *pendingStore) put(token string, data webauthn.SessionData, kind, userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	p.entries[token] = pendingEntry{
		data:    data,
		kind:    kind,
		userID:  userID,
		expires: time.Now().Add(pendingTTL),
	}
}

func (p *pendingStore) take(token string) (pendingEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	e, ok := p.entries[token]
	if !ok {
		return pendingEntry{}, false
	}
	delete(p.entries, token)
	if time.Now().After(e.expires) {
		return pendingEntry{}, false
	}
	return e, true
}

func (p *pendingStore) gcLocked() {
	now := time.Now()
	for k, e := range p.entries {
		if now.After(e.expires) {
			delete(p.entries, k)
		}
	}
}
