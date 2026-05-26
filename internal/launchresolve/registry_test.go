package launchresolve

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"
)

// fixtureRoot is the in-repo fixture catalog. Tests resolve against it so
// they do not depend on the live ~/.tether/catalog/.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "catalog"))
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}
	return abs
}

func openFixture(t *testing.T) *Registry {
	t.Helper()
	reg, err := OpenAt(Options{CatalogRoot: fixtureRoot(t)})
	if err != nil {
		t.Fatalf("OpenAt fixture: %v", err)
	}
	return reg
}

func TestOpenAt_IngestReport(t *testing.T) {
	reg := openFixture(t)
	rep := reg.Report()

	if !rep.Reconciles() {
		t.Errorf("ingest report does not reconcile: %+v", rep)
	}

	wantByKind := map[agentlaunch.RegistryKind]int{
		agentlaunch.RegistryKindAgentSource:       1,
		agentlaunch.RegistryKindRuntimeBinding:    3,
		agentlaunch.RegistryKindMCPServer:         1,
		agentlaunch.RegistryKindBootSpec:          1,
		agentlaunch.RegistryKindExecutionTemplate: 1,
	}
	for kind, want := range wantByKind {
		if got := rep.RegisteredByKind[kind]; got != want {
			t.Errorf("RegisteredByKind[%s] = %d, want %d", kind, got, want)
		}
	}
	if got := rep.Registered(); got != 7 {
		t.Errorf("total Registered() = %d, want 7", got)
	}

	// projects/ has no registry kind: its one file must be an unmapped
	// skip, never an invented kind.
	var unmapped, nonYAML int
	for _, s := range rep.Skipped {
		switch s.Reason {
		case agentlaunch.IngestSkipUnmappedDir:
			unmapped++
			if s.Subdir != "projects" && s.Subdir != "sandbox-profiles" {
				t.Errorf("unexpected unmapped subdir %q", s.Subdir)
			}
		case agentlaunch.IngestSkipNonYAML:
			nonYAML++
		}
	}
	if unmapped != 1 {
		t.Errorf("unmapped-directory skips = %d, want 1 (projects/tether.yaml)", unmapped)
	}
	if nonYAML != 1 {
		t.Errorf("non-yaml-file skips = %d, want 1 (the .bak file)", nonYAML)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("ingest errors = %+v, want none", rep.Errors)
	}
}

func TestOpenAt_UnreadableRootIsHardError(t *testing.T) {
	_, err := OpenAt(Options{CatalogRoot: filepath.Join(fixtureRoot(t), "does-not-exist")})
	if err == nil {
		t.Fatal("expected hard error for unreadable catalog root, got nil")
	}
}

func TestRegistry_Health(t *testing.T) {
	reg := openFixture(t)
	h, err := reg.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != agentlaunch.HealthStatusOK {
		t.Errorf("health status = %q, want ok", h.Status)
	}
	if st := reg.Status(); st.Degraded {
		t.Errorf("fresh registry reports degraded: %+v", st)
	}
}
