package main

// whoami_test.go — integration coverage for `mux whoami` (T08). Reuses
// registry_test.go's newFixture (real api.Handler + registry.Service
// over an in-memory SQLite, registryClientFactory pointed at it).

import (
	"context"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func resetWhoamiFlags() {
	whoamiAs = ""
	whoamiJSON = false
}

func TestWhoami_RequiresAs_Exit2(t *testing.T) {
	_ = newFixture(t)
	t.Cleanup(resetWhoamiFlags)
	err := whoamiCmd.RunE(whoamiCmd, nil)
	if err == nil {
		t.Fatal("expected a validation error with no --as")
	}
	assertExitCode(t, err, 2)
}

func TestWhoami_UnregisteredActor_NoErrorPrintsNoneEverywhere(t *testing.T) {
	_ = newFixture(t)
	t.Cleanup(resetWhoamiFlags)
	whoamiAs = "msg://session/agent-mux/sess_never_registered"

	out := captureRegistryStdout(t, func() {
		if err := whoamiCmd.RunE(whoamiCmd, nil); err != nil {
			t.Fatalf("whoami: %v", err)
		}
	})
	for _, want := range []string{"profile: (not registered)", "external_ids: (none)", "groups: (none)", "binding: (none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

func TestWhoami_RegisteredActor_PrintsProfileAndBinding(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(resetWhoamiFlags)
	agent := f.seedAgent("Worker", "engineer")
	if _, err := f.svc.LeaseBinding(context.Background(), agent.URN, "sess-1", "local", "sess-1", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("lease binding: %v", err)
	}

	whoamiAs = agent.URN
	out := captureRegistryStdout(t, func() {
		if err := whoamiCmd.RunE(whoamiCmd, nil); err != nil {
			t.Fatalf("whoami: %v", err)
		}
	})
	if !strings.Contains(out, "profile: Worker") {
		t.Errorf("output missing profile line: %s", out)
	}
	if !strings.Contains(out, "binding: session_id=sess-1") {
		t.Errorf("output missing binding line: %s", out)
	}
}
