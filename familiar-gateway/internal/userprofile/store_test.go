package userprofile

import "testing"

// NewStore rejects a nil pool — the only logic worth a unit test in
// this package. Get/Set are thin SQL wrappers exercised by the
// DSN-gated integration tests in internal/db.
func TestNewStoreNilPool(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) = nil error, want error")
	}
}
