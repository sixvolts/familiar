package admin

import "testing"

// IsAdmin is the single gate the whole console leans on for "shards
// never inherit admin". A shard session hydrates its Role from the
// owner, so a shard owned by an admin has Role=="admin" — but its
// permission envelope's CanAdmin is false unless the shard opted in.
// The media handlers had hand-rolled `au.Role != "admin"` and so
// bypassed book-membership for such a shard; this pins the invariant
// they now depend on.
func TestIsAdmin_ShardOwnedByAdminIsNotAdmin(t *testing.T) {
	cases := []struct {
		name string
		au   AuthUser
		want bool
	}{
		{"plain admin user", AuthUser{Role: "admin"}, true},
		{"plain non-admin user", AuthUser{Role: "user"}, false},
		{
			"shard owned by admin, envelope withholds admin",
			AuthUser{Role: "admin", PrincipalType: PrincipalTypeShard,
				Permissions: &SessionPermissions{CanAdmin: false}},
			false,
		},
		{
			"shard explicitly granted admin",
			AuthUser{Role: "admin", PrincipalType: PrincipalTypeShard,
				Permissions: &SessionPermissions{CanAdmin: true}},
			true,
		},
	}
	for _, c := range cases {
		if got := c.au.IsAdmin(); got != c.want {
			t.Errorf("%s: IsAdmin()=%v, want %v", c.name, got, c.want)
		}
	}
}
