package app

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// codexCountingAgent is a stand-in codex that records each run and answers.
func codexCountingAgent(counter string) string {
	return "#!/bin/sh\necho run >> \"" + counter + "\"\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"RAN\"}}'\necho '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
}

// A catalog problem must never take out an unprotected Codex launch. Codex ships as
// not protected, so protectionPlan lets it through, and session create answered 201;
// but the launch then asked the same protection plan for the directories the planted
// MCP proxy must not write, and a project whose repo_root runs through a file (an
// UnprotectableLayerError) failed that launch with a bare 500, the session left
// failed, though the comment there says Codex is excepted. The proxy is confined as
// far as the catalog allows, the layer that cannot be protected is left out and
// reported, and the launch runs. Claude, on the same catalog, is still refused, and
// with its own code: the fix is the catalog entry, not bubblewrap.
func TestACodexLaunchIsNotRefusedForAnotherProjectsBrokenCatalogEntry(t *testing.T) {
	if codexProtectionMode != CodexNotProtected {
		t.Fatalf("the build ships codexProtectionMode = %q; want %q while CW-20261001-0230 is open", codexProtectionMode, CodexNotProtected)
	}
	svc, _, _ := tetherLayoutKeepingMode(t)
	clearWritableRoots(t)
	base := t.TempDir()
	live := filepath.Join(base, "live-repo")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: live}, "broken": {RepoRoot: filepath.Join(file, "child")}}

	var logged bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	err := svc.refuseUnprotectable(&launch.Plan{ProviderBrand: "claude", ProjectID: "proj"}, "cli")
	if !errors.Is(err, launch.ErrProjectLayerUnprotectable) || errors.Is(err, launch.ErrProtectionUnavailable) {
		t.Fatalf("claude on a broken catalog: err = %v; want launch.ErrProjectLayerUnprotectable, not the bubblewrap sentinel", err)
	}

	counter := filepath.Join(t.TempDir(), "runs")
	sessID, _, err := launchFakeCodex(t, svc, codexCountingAgent(counter), nil)
	if err != nil {
		t.Fatalf("a codex launch was refused for another project's broken catalog entry: %v", err)
	}
	if err := svc.SendTurn(context.Background(), sessID, "go"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(counter); strings.Contains(string(data), "run") { //nolint:gosec // test-owned path
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if data, _ := os.ReadFile(counter); !strings.Contains(string(data), "run") { //nolint:gosec // test-owned path
		t.Fatal("the codex agent never ran")
	}
	for _, want := range []string{`project "broken"`, "LEFT OPEN", "a file is in the way"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("the layer left open was not reported (missing %q):\n%s", want, logged.String())
		}
	}
}

// What is wrong with the launching project's own root is that launch's problem, for
// Codex too: the typed refusal, not a bare internal error, and the session is not
// left behind as a mystery.
func TestACodexLaunchForAProjectWithAMissingRepoRootIsTheTypedRefusal(t *testing.T) {
	svc, _, _ := tetherLayoutKeepingMode(t)
	clearWritableRoots(t)
	gone := filepath.Join(t.TempDir(), "gone-repo")
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: gone}}
	_, _, err := launchFakeCodex(t, svc, codexCountingAgent(filepath.Join(t.TempDir(), "runs")), nil)
	var rootErr *config.ProjectRootError
	if !errors.Is(err, launch.ErrLaunchProjectRootMissing) || !errors.As(err, &rootErr) || rootErr.Project != "proj" {
		t.Fatalf("err = %v; want launch.ErrLaunchProjectRootMissing wrapping a ProjectRootError for proj", err)
	}
	if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
		t.Fatalf("the refused launch created the project's root (%v)", statErr)
	}
}
