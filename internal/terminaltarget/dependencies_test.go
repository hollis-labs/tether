package terminaltarget

import (
	"os/exec"
	"strings"
	"testing"
)

// Check the entire dependency graph: host calls under the lease lock deadlock.
func TestResolverDoesNotImportChatHost(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput() //nolint:gosec // fixed argv
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "libs/agent/sdk/chathost") || strings.HasSuffix(pkg, "/internal/chatwire") || strings.HasSuffix(pkg, "/internal/terminalattach") {
			t.Errorf("terminal target resolver depends on host adapter %s", pkg)
		}
	}
	if !strings.Contains(string(out), "github.com/hollis-labs/tether/internal/conversation") {
		t.Fatal("dependency graph is missing the conversation mapping resolver")
	}
}
