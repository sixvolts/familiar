package identity

import "testing"

// DeriveCanonicalID is pure — no DB, no resolver state — so it
// carries the bulk of bootstrap testing. The DB-backed methods
// (BootstrapSlackUser, SetUserEmail) follow the existing pattern
// in this package of skipping unit tests when they require a live
// pool; integration coverage lives at the slack-adapter smoke
// level once a real workspace is available.
func TestDeriveCanonicalID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Sam Smith", "sam_smith"},
		{"ALISON", "alison"},
		{"Ada Lovelace", "ada_lovelace"},
		{"al.squirrel", "al_squirrel"},
		{"multi  space", "multi_space"}, // runs collapse
		{"_leading_and_trailing_", "leading_and_trailing"},
		{"hyphen-name", "hyphen_name"},
		{"unicode é ñ", "unicode"},     // accents dropped, space separator preserved once
		{"🙂 only emoji", "only_emoji"}, // leading emoji dropped
		{"🙂🙂🙂", "user"},                // all-emoji falls back
		{"", "user"},                   // empty falls back
		{"  ", "user"},                 // whitespace-only falls back
		{"123abc", "123abc"},           // digits allowed
		{"Foo.Bar.Baz", "foo_bar_baz"}, // dots as separators
		{"Name    With    Many    Spaces", "name_with_many_spaces"},
	}
	for _, tc := range cases {
		got := DeriveCanonicalID(tc.in)
		if got != tc.want {
			t.Errorf("DeriveCanonicalID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
