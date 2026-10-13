package catalogcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exampleCatalog copies examples/catalog, which is a valid catalog, so a test
// can break it.
func exampleCatalog(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "catalog")
	src, err := filepath.Abs(filepath.Join("..", "..", "examples", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-r", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("copy example catalog: %v\n%s", err, out)
	}
	return dst
}

func find(r Report, status string) []Result {
	var out []Result
	for _, c := range r.Results {
		if c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

func TestRunValidCatalog(t *testing.T) {
	rep := Run(exampleCatalog(t))
	if !rep.OK || rep.Failures != 0 {
		t.Fatalf("example catalog should validate: %+v", find(rep, StatusFail))
	}
	if len(rep.Results) < 3 {
		t.Fatalf("expected several checks, got %+v", rep.Results)
	}
}

func TestRunReportsABrokenReferenceWithItsEntity(t *testing.T) {
	root := exampleCatalog(t)
	p := filepath.Join(root, "launches", "demo-launch.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "agent: demo-agent", "agent: no-such-agent", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Run(root)
	if rep.OK || rep.Failures == 0 {
		t.Fatalf("a launch naming a missing agent should fail: %+v", rep)
	}
	fails := find(rep, StatusFail)
	if rep.Results[0].Status != StatusFail || !strings.Contains(fails[0].Message, "no-such-agent") {
		t.Fatalf("failures should come first and name the agent: %+v", rep.Results)
	}
}

func TestRunMissingCatalog(t *testing.T) {
	rep := Run(filepath.Join(t.TempDir(), "nope"))
	if rep.OK || len(rep.Results) != 1 || rep.Results[0].Name != "catalog-present" {
		t.Fatalf("report = %+v", rep)
	}
}

func TestRunDoesNotModifyTheCatalog(t *testing.T) {
	root := exampleCatalog(t)
	snap := func() string {
		var sb strings.Builder
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				b, _ := os.ReadFile(p)
				sb.WriteString(p + string(b))
			}
			return nil
		})
		return sb.String()
	}
	before := snap()
	Run(root)
	if snap() != before {
		t.Fatal("validation changed the catalog")
	}
}

func TestEntityOf(t *testing.T) {
	for msg, want := range map[string]string{
		`launch "x" references unknown agent "y"`: "launch:x",
		`agent "ops" has unknown mcp_scopes`:      "agent:ops",
		`nothing named here`:                      "",
	} {
		if got := entityOf(msg); got != want {
			t.Errorf("entityOf(%q) = %q, want %q", msg, got, want)
		}
	}
}
