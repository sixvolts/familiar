package admin

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// A corrupt / half-written credential_blob deserializes to a zero
// Credential with an empty ID. Login is a shared ceremony (loginBegin
// offers every credential as an allow-list entry), so one such row left
// in the list can break BeginLogin for every user. usableCredentials is
// the guard that drops them; pin it.
func TestUsableCredentials_DropsEmptyID(t *testing.T) {
	creds := []webauthn.Credential{
		{ID: []byte("real-1")},
		{ID: nil},      // corrupt blob → empty id
		{ID: []byte{}}, // also empty
		{ID: []byte("real-2")},
	}
	got, dropped := usableCredentials(creds)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d credentials, want 2", len(got))
	}
	for _, c := range got {
		if len(c.ID) == 0 {
			t.Error("kept a credential with an empty ID")
		}
	}
}

func TestUsableCredentials_AllValid(t *testing.T) {
	creds := []webauthn.Credential{{ID: []byte("a")}, {ID: []byte("b")}}
	got, dropped := usableCredentials(creds)
	if dropped != 0 || len(got) != 2 {
		t.Errorf("got %d kept / %d dropped, want 2/0", len(got), dropped)
	}
}
