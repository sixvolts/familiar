package sidecar

import (
	"reflect"
	"testing"
)

func TestParseExpandedQueries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain lines",
			in:   "gpu-host hardware specs\ngpu-host IP address\ngpu-host network config",
			want: []string{"gpu-host hardware specs", "gpu-host IP address", "gpu-host network config"},
		},
		{
			name: "numbered list is stripped",
			in:   "1. first query\n2. second query\n3) third query",
			want: []string{"first query", "second query", "third query"},
		},
		{
			name: "bullets stripped",
			in:   "- alpha\n* beta\n  - gamma",
			want: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "quotes stripped",
			in:   "\"gpu-host IP\"\n'network topology'",
			want: []string{"gpu-host IP", "network topology"},
		},
		{
			name: "dedup",
			in:   "gpu-host\ngpu-host\ngpu-host network",
			want: []string{"gpu-host", "gpu-host network"},
		},
		{
			name: "blank lines dropped",
			in:   "\n\nalpha\n\nbeta\n\n",
			want: []string{"alpha", "beta"},
		},
		{
			name: "single line fallback",
			in:   "hey",
			want: []string{"hey"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseExpandedQueries(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
