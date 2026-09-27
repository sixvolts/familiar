package admin

import "testing"

// A kiosk's page-event stream carries only its envelope's books; it got
// every book its owner belongs to.
func TestPageEventAllowed(t *testing.T) {
	member := map[string]bool{"b-kitchen": true, "b-private": true}
	kiosk := AuthUser{UserID: "owner", Role: "user", PrincipalType: PrincipalTypeShard,
		Permissions: &SessionPermissions{Books: []string{"b-kitchen"}}}
	adminKiosk := AuthUser{UserID: "boss", Role: "admin", PrincipalType: PrincipalTypeShard,
		Permissions: &SessionPermissions{Books: []string{"b-kitchen"}, CanAdmin: true}}
	user := AuthUser{UserID: "owner", Role: "user"}
	admin := AuthUser{UserID: "boss", Role: "admin"}
	for _, tc := range []struct {
		name string
		au   AuthUser
		book string
		want bool
	}{
		{"kiosk, its book", kiosk, "b-kitchen", true},
		{"kiosk, the owner's other book", kiosk, "b-private", false},
		{"admin-enabled kiosk, outside its books", adminKiosk, "b-elsewhere", false},
		{"user, a member", user, "b-private", true},
		{"user, not a member", user, "b-elsewhere", false},
		{"admin, any book", admin, "b-elsewhere", true},
	} {
		if got := pageEventAllowed(tc.au, member, tc.book); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
