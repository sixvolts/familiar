package main

// Workspace main-package tests. Currently covers the
// /admin → /console back-compat redirect that moved here from the
// gateway in FAMILIAR-WORKSPACE-SPEC Phase 0. Bookmarks from the
// pre-rename era land on the workspace's hostname now; the gateway
// no longer sees them.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectAdminToConsole_PreservesPath(t *testing.T) {
	cases := map[string]string{
		"/admin/":                         "/console/",
		"/admin":                          "/console/",
		"/admin/api/auth/status":          "/console/api/auth/status",
		"/admin/api/shards/charger":       "/console/api/shards/charger",
		"/admin/api/users?status=pending": "/console/api/users?status=pending",
	}
	for in, want := range cases {
		req := httptest.NewRequest("GET", in, nil)
		rec := httptest.NewRecorder()
		redirectAdminToConsole(rec, req)
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status = %d, want 301", in, rec.Code)
			continue
		}
		got := rec.Header().Get("Location")
		if got != want {
			t.Errorf("%s: Location = %q, want %q", in, got, want)
		}
	}
}
