package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The shells' CSP keeps forms from posting off-site: sanitized content
// can't carry a <form> any more, and this is the layer behind that.
func TestDocSecurityHeaders_FormActionSelf(t *testing.T) {
	w := httptest.NewRecorder()
	docSecurityHeaders(w)
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("CSP lacks form-action 'self': %s", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP lost frame-ancestors: %s", csp)
	}
}
