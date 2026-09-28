package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// When re-executed with this variable set, the test binary runs the
// gateway's main() instead of the tests (see runGatewayMain).
const gatewayMainEnv = "FAMILIAR_TEST_RUN_GATEWAY_MAIN"

func init() {
	if os.Getenv(gatewayMainEnv) != "1" {
		return
	}
	dbStartupWait = 2 * time.Second
	os.Args = []string{"familiar-gateway", "--http", "--config", os.Getenv("FAMILIAR_TEST_GATEWAY_CONFIG")}
	main()
	os.Exit(0)
}

// A gateway whose configured database is unreachable must exit
// non-zero. It used to log a warning and serve on without a database
// (no login, no chat auth) while /api/health said ok, and it exited 0
// on every failure, which systemd's Restart=on-failure never restarts.
func TestGatewayExitsNonZeroWhenTheDatabaseIsUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("re-executes the gateway")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.toml")
	cfg := fmt.Sprintf(`
[adapter.http]
listen_addr = "127.0.0.1:0"

[skills]
dir = %q

[memory]
local_dsn = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"

[admin]
enabled = false

[sidecar]
enabled = false

[router]
enabled = false

[[models]]
id = "test/dummy"
endpoint = "http://127.0.0.1:1"
role = "small"
`, filepath.Join(dir, "skills"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// A gateway that serves on without its database never exits, so
	// bound the wait: timing out is the failure being tested for.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), gatewayMainEnv+"=1", "FAMILIAR_TEST_GATEWAY_CONFIG="+cfgPath, "FAMILIAR_HOME="+dir)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("gateway kept running with no database for 60s; output:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("gateway exited with %v, want a non-zero exit; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "database unreachable") {
		t.Errorf("exit wasn't for the unreachable database; output:\n%s", out)
	}
}
