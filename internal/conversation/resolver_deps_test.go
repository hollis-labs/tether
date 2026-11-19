package conversation

import (
	"os/exec"
	"strings"
	"testing"
)

// The resolver runs under the chat host's lease lock, so it must not reach
// the chat host, directly or through any dependency.
func TestResolverDoesNotImportChatHost(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", ".").CombinedOutput() //nolint:gosec // G204: fixed argv
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "libs/agent/sdk/chathost") || strings.HasSuffix(pkg, "/internal/terminalattach") {
			t.Fatalf("conversation resolver depends on %s", pkg)
		}
	}
	if !strings.Contains(string(out), "github.com/hollis-labs/tether/internal/store") {
		t.Fatal("go list -deps did not list the store; the check is not looking at the right package")
	}
}
