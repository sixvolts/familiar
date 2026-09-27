package admin

import (
	"fmt"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// The store is bounded: /login/begin is public, and it used to grow by
// one five-minute entry per call, with a sweep over all of them on
// every put and take.
func TestPendingStore_Bounded(t *testing.T) {
	p := newPendingStore()
	for i := 0; i < 3*maxPending; i++ {
		p.put(fmt.Sprintf("login-%d", i), webauthn.SessionData{}, PendingKindLogin, "")
	}
	if n := len(p.entries); n > maxPending {
		t.Fatalf("%d entries after a flood, want at most %d", n, maxPending)
	}
	// The newest are kept; a flood evicts the oldest first.
	if _, ok := p.take(fmt.Sprintf("login-%d", 3*maxPending-1)); !ok {
		t.Error("the newest login was evicted")
	}
}

// A flood of anonymous logins displaces other logins, not a signed-in
// user's registration in progress.
func TestPendingStore_FloodKeepsRegistrations(t *testing.T) {
	p := newPendingStore()
	p.put("reg", webauthn.SessionData{}, PendingKindRegister, "bob")
	for i := 0; i < 2*maxPending; i++ {
		p.put(fmt.Sprintf("login-%d", i), webauthn.SessionData{}, PendingKindLogin, "")
	}
	if e, ok := p.take("reg"); !ok || e.userID != "bob" {
		t.Error("a login flood evicted a registration in progress")
	}
}
