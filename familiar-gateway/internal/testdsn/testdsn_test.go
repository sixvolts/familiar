package testdsn

import (
	"strings"
	"testing"

	"github.com/lib/pq"
)

// effectiveOptions is what lib/pq will actually send as options.
func effectiveOptions(t *testing.T, dsn string) string {
	t.Helper()
	conn, err := pq.ParseURL(dsn)
	if err != nil {
		t.Fatalf("pq.ParseURL: %v", err)
	}
	// conn is key='value' pairs; a value may hold spaces and \' escapes.
	i := strings.Index(conn, "options='")
	if i < 0 {
		return ""
	}
	rest := conn[i+len("options='"):]
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++
		case '\'':
			return "options='" + rest[:j] + "'"
		}
	}
	t.Fatalf("unterminated options in %q", conn)
	return ""
}

func TestWithSearchPath(t *testing.T) {
	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"bare", "postgres://u:pw@localhost:5432/familiar_test",
			`options='-csearch_path=memory_test,public'`},
		{"other params kept", "postgres://u:pw@localhost:5432/familiar_test?sslmode=disable",
			`options='-csearch_path=memory_test,public'`},
		// The documented DSN form: its own search_path must not win.
		{"existing search_path replaced", "postgres://u:pw@localhost:5432/familiar_test?sslmode=disable&options=-csearch_path%3De2e_test%2Cpublic",
			`options='-csearch_path=memory_test,public'`},
		{"spaced -c form replaced, other settings kept", "postgres://u:pw@localhost/db?options=-c%20statement_timeout%3D5000%20-c%20search_path%3De2e_test",
			`options='-c statement_timeout=5000 -csearch_path=memory_test,public'`},
		{"long form replaced", "postgres://u:pw@localhost/db?options=--search_path%3De2e_test",
			`options='-csearch_path=memory_test,public'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithSearchPath(tc.dsn, "memory_test")
			if err != nil {
				t.Fatal(err)
			}
			if eff := effectiveOptions(t, got); eff != tc.want {
				t.Errorf("lib/pq sends %s, want %s", eff, tc.want)
			}
			if tc.name == "other params kept" && !strings.Contains(got, "sslmode=disable") {
				t.Errorf("sslmode dropped: %s", got)
			}
		})
	}
}

func TestWithSearchPathRefusesWithoutLeaking(t *testing.T) {
	for _, dsn := range []string{
		"host=localhost user=u password=hunter2 dbname=familiar_test",
		"postgres://u:hunter2@localhost:bad port/db",
	} {
		_, err := WithSearchPath(dsn, "s")
		if err == nil {
			t.Fatalf("accepted %q", dsn)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}
