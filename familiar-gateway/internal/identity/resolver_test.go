package identity

import (
	"context"
	"testing"
)

func TestResolverNilReportsUnmapped(t *testing.T) {
	var r *Resolver
	canonical, ok := r.Resolve("slack", "U123")
	if ok || canonical != "" {
		t.Errorf("nil Resolver.Resolve = (%q, %v), want (\"\", false)", canonical, ok)
	}
}

func TestResolverUnmappedReturnsFalse(t *testing.T) {
	r, err := NewResolver(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	// OWNER-MIGRATION: an unmapped identity must surface as
	// (`""`, false) rather than falling back to a default. The
	// caller is expected to reject the request rather than silently
	// route it to the bootstrap admin's data.
	if canonical, ok := r.Resolve("slack", "U_UNKNOWN"); ok || canonical != "" {
		t.Errorf("unknown identity resolved to (%q, %v), want (\"\", false)", canonical, ok)
	}
}

func TestResolverRegisterAndResolve(t *testing.T) {
	r, err := NewResolver(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if err := r.Register(context.Background(), "slack", "U00000000000", "operator", "Operator"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if canonical, ok := r.Resolve("slack", "U00000000000"); !ok || canonical != "operator" {
		t.Errorf("after Register, Resolve = (%q, %v), want (\"operator\", true)", canonical, ok)
	}
	if r.Count() != 1 {
		t.Errorf("Count = %d, want 1", r.Count())
	}

	if err := r.Register(context.Background(), "slack", "U_ALISON", "alison", "Ali"); err != nil {
		t.Fatalf("Register alison: %v", err)
	}
	if canonical, ok := r.Resolve("slack", "U_ALISON"); !ok || canonical != "alison" {
		t.Errorf("alison mapping resolved to (%q, %v), want (\"alison\", true)", canonical, ok)
	}
	if canonical, ok := r.Resolve("slack", "U00000000000"); !ok || canonical != "operator" {
		t.Errorf("operator mapping collided with alison: got (%q, %v)", canonical, ok)
	}
}

func TestResolverEmptyInputsReturnFalse(t *testing.T) {
	r, _ := NewResolver(context.Background(), nil)
	if canonical, ok := r.Resolve("", "U123"); ok || canonical != "" {
		t.Errorf("empty platform Resolve = (%q, %v), want (\"\", false)", canonical, ok)
	}
	if canonical, ok := r.Resolve("slack", ""); ok || canonical != "" {
		t.Errorf("empty platform_id Resolve = (%q, %v), want (\"\", false)", canonical, ok)
	}
}

func TestResolverRegisterValidation(t *testing.T) {
	r, _ := NewResolver(context.Background(), nil)
	if err := r.Register(context.Background(), "", "U1", "operator", ""); err == nil {
		t.Error("Register with empty platform should error")
	}
	if err := r.Register(context.Background(), "slack", "", "operator", ""); err == nil {
		t.Error("Register with empty platform_id should error")
	}
	if err := r.Register(context.Background(), "slack", "U1", "", ""); err == nil {
		t.Error("Register with empty canonical_id should error")
	}
}
