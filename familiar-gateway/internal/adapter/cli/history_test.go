package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// history_file = "cli_history" (no directory) panicked at startup.
func TestEnsureHistoryDir(t *testing.T) {
	ensureHistoryDir("cli_history") // must not panic
	nested := filepath.Join(t.TempDir(), "a", "b", "history")
	ensureHistoryDir(nested)
	if st, err := os.Stat(filepath.Dir(nested)); err != nil || !st.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}
