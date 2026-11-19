package mcptransport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
)

// TestBackendConsumerIntegration runs a separately compiled gateway contract
// test against real Tether identity, policy, native handlers and a disposable
// Unix socket. The two applications keep their own tests and wire adapters;
// neither imports the other's internal implementation or needs a shared test
// library. Opt in with TETHER_MCP_CONSUMER_TEST_BINARY=/absolute/test/binary.
func TestBackendConsumerIntegration(t *testing.T) {
	binary := os.Getenv("TETHER_MCP_CONSUMER_TEST_BINARY")
	if binary == "" {
		t.Skip("set TETHER_MCP_CONSUMER_TEST_BINARY to run the paired gateway integration")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("consumer test binary must be an absolute path")
	}
	g := newBackendFixture(t)
	token, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	local := g.f.token(t, "consumer", []string{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestRealTetherBackend$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		"TETHER_TEST_BACKEND_SOCKET="+strings.TrimPrefix(g.addr, "unix:"),
		"TETHER_TEST_CALLER_TOKEN="+token,
		"TETHER_TEST_SESSION_ID=s-rot",
		"TETHER_TEST_LOCAL_TOKEN="+local,
	)
	output, err := cmd.CombinedOutput()
	// Even failed consumer assertions must not print the fixture credentials.
	report := strings.ReplaceAll(strings.ReplaceAll(string(output), token, "[fixture credential]"), local, "[fixture credential]")
	if len(report) > 32<<10 {
		report = report[len(report)-(32<<10):]
	}
	if err != nil {
		t.Fatalf("consumer integration: %v\n%s", err, report)
	}
	// A typo in the requested test name must not turn an empty run into evidence.
	if !strings.Contains(report, "--- PASS: TestRealTetherBackend") {
		t.Fatalf("consumer test did not pass (missing test or skipped):\n%s", report)
	}
	t.Log(report)
}
