package admin

import (
	"encoding/base64"

	"github.com/go-webauthn/webauthn/webauthn"
)

// adminUser implements webauthn.User.
//
// WebAuthnID() returns the WebAuthn user handle — the opaque id baked
// into the authenticator at registration and echoed back on every
// assertion. It is held in `handle`, SEPARATE from `id` (the canonical
// user id used for sessions / identity), so a canonical-id rename
// can't invalidate an existing passkey. `handle` is empty only for the
// loginBegin discoverable-ceremony placeholder, where WebAuthnID isn't
// checked against a device; it falls back to `id` there.
//
// EncodeUserIDAsString is enabled on the WebAuthn config so browsers
// see the handle as a readable string instead of a base64url blob.
type adminUser struct {
	id          string
	handle      string
	displayName string
	credentials []webauthn.Credential
}

func (u *adminUser) WebAuthnID() []byte {
	if u.handle != "" {
		return []byte(u.handle)
	}
	return []byte(u.id)
}
func (u *adminUser) WebAuthnName() string                       { return u.id }
func (u *adminUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *adminUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func encodeCredentialID(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}
