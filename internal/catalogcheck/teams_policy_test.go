package catalogcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "teams-policy.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTeamsPolicyChecks(t *testing.T) {
	root := exampleCatalog(t)
	if got := Run(root); len(find(got, StatusFail)) != 0 {
		t.Fatalf("baseline fails: %+v", find(got, StatusFail))
	}

	// Two launches sharing one pin: Parse rejects it, so validate must fail.
	writePolicy(t, root, "version: 1\nlaunches:\n  - launch: demo-launch\n    runtime: claude\n"+
		"    definition: {id: d, revision: r1, digest: x}\n  - launch: api-stub-launch\n    runtime: claude\n    definition: {id: d, revision: r1, digest: x}\n")
	rep := Run(root)
	fails := find(rep, StatusFail)
	if len(fails) != 1 || fails[0].Name != "teams-policy" || !strings.Contains(fails[0].Message, "already bound") {
		t.Fatalf("want teams-policy pin failure, got %+v", fails)
	}

	// A valid policy naming a disabled launch and a missing one warns.
	disabled := filepath.Join(root, "launches", "demo-launch.yaml")
	b, err := os.ReadFile(disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disabled, append(b, []byte("\nenabled: false\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	writePolicy(t, root, "version: 1\nlaunches:\n  - launch: demo-launch\n    runtime: claude\n"+
		"    definition: {id: d1, revision: r1, digest: x1}\n  - launch: ghost\n    runtime: claude\n    definition: {id: d2, revision: r1, digest: x2}\n")
	rep = Run(root)
	if len(find(rep, StatusFail)) != 0 {
		t.Fatalf("unexpected failures: %+v", find(rep, StatusFail))
	}
	var msgs []string
	for _, w := range find(rep, StatusWarn) {
		if w.Name == "teams-policy-launch" {
			msgs = append(msgs, w.Message)
		}
	}
	joined := strings.Join(msgs, "|")
	if !strings.Contains(joined, `"demo-launch" is disabled`) || !strings.Contains(joined, `"ghost" is not in the catalog`) {
		t.Fatalf("warnings = %q", joined)
	}
}
