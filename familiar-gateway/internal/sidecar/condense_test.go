package sidecar

import (
	"context"
	"testing"
)

func TestParseCondensed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "gpu-host server IP address", "gpu-host server IP address"},
		{"first line only", "Postgres default port\nignored second line", "Postgres default port"},
		{"quotes stripped", "\"gpu-host IP\"", "gpu-host IP"},
		{"echoed label stripped", "Rewritten query: gpu-host IP address", "gpu-host IP address"},
		{"echoed label case-insensitive", "REWRITTEN QUERY: alpha", "alpha"},
		{"surrounding whitespace", "  trimmed query  ", "trimmed query"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCondensed(tc.in); got != tc.want {
				t.Errorf("parseCondensed(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// With no history there's nothing to resolve against — CondenseQuery
// must short-circuit and return the raw message without a network
// round-trip (so a nil-endpoint router doesn't error).
func TestCondenseQueryNoHistoryShortCircuits(t *testing.T) {
	r := &HTTPRouter{}
	got, err := r.CondenseQuery(context.Background(), nil, "what about the timeout?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "what about the timeout?" {
		t.Errorf("got %q, want the raw message back", got)
	}
}
