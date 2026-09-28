// Package testdsn scopes the test database DSN to one schema, so each
// DB-backed test package works in its own schema and can truncate its
// tables without touching another package's rows.
//
// It imports nothing from the gateway, so any package's tests can use
// it, internal/db's included (internal/testutil imports internal/db).
package testdsn

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// Scoped returns dsn with its search_path set to "<schema>,public", or
// fails the test. The DSN itself is never printed: it carries a password.
func Scoped(t testing.TB, dsn, schema string) string {
	t.Helper()
	s, err := WithSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("scoping FAMILIAR_TEST_DSN to schema %s: %v", schema, err)
	}
	return s
}

// WithSearchPath returns dsn, a postgres:// URL, with search_path set to
// "<schema>,public" in its options parameter.
//
// Any search_path the DSN already sets is replaced, and its other
// options are kept. Appending a second options= instead (what the tests
// used to do) does nothing when the DSN already has one: lib/pq reads
// the first, so every package would share the DSN's schema and truncate
// each other's rows.
func WithSearchPath(dsn, schema string) (string, error) {
	if !strings.Contains(dsn, "://") {
		return "", errors.New("must be a postgres:// URL (key=value DSNs are not supported)")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		// Not err itself: url.Error quotes the whole URL, password included.
		return "", errors.New("not a valid URL")
	}
	q := u.Query()
	var keep []string
	for _, opts := range q["options"] {
		fields := strings.Fields(opts)
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			switch {
			case f == "-c" && i+1 < len(fields) && isSearchPath(fields[i+1]):
				i++
			case isSearchPath(strings.TrimPrefix(f, "-c")),
				isSearchPath(strings.TrimPrefix(f, "--")):
			default:
				keep = append(keep, f)
			}
		}
	}
	keep = append(keep, "-csearch_path="+schema+",public")
	q.Set("options", strings.Join(keep, " "))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func isSearchPath(setting string) bool {
	return strings.HasPrefix(setting, "search_path=")
}
