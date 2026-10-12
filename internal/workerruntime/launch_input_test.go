package workerruntime_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// The hub is the only commander, but a launch spec still carries values that reach a process
// launcher and git. These pin the two inputs that must not be able to change what runs.

func launchFrame(t *testing.T, spec workerwire.LaunchSpec) workerwire.Frame {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return workerwire.Frame{T: "launch", CmdID: "cmd-1", Session: spec.Session, Spec: raw}
}

func TestLaunchRefusesProviderThatIsAPath(t *testing.T) {
	e := workerruntime.NewExecutor(t.TempDir(), "unused", func(workerwire.Frame) error { return nil })
	for _, provider := range []string{"/bin/sh", "../claude", `..\claude`, "bin/claude", ""} {
		err := e.Command(context.Background(), launchFrame(t, workerwire.LaunchSpec{Session: "s1", Provider: provider}))
		if err == nil || !strings.Contains(err.Error(), "provider must be a bare command name") {
			t.Fatalf("provider %q: err = %v, want a refusal", provider, err)
		}
	}
}

func TestLaunchRefusesGitRefThatLooksLikeAnOption(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", src}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	e := workerruntime.NewExecutor(t.TempDir(), "unused", func(workerwire.Frame) error { return nil })
	err := e.Command(context.Background(), launchFrame(t, workerwire.LaunchSpec{
		Session:   "s2",
		Provider:  "git",
		Workspace: workerwire.Workspace{Kind: "git", URL: src, Ref: "--orphan=evil"},
	}))
	if err == nil || !strings.Contains(err.Error(), "invalid git ref") {
		t.Fatalf("err = %v, want invalid git ref", err)
	}
}
