package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/launch"
)

// CW-20261001-0263: the state directory, the operator token and the secrets
// directory are unreadable to every agent Tether wraps.

// hiddenLayout adds the operator token and a secrets directory to a Tether
// layout and returns the canonical paths of the state directory, the token and
// the secrets directory.
func hiddenLayout(t *testing.T, catalog, run string) (state, token, secrets string) {
	t.Helper()
	root := filepath.Dir(catalog)
	state = filepath.Join(root, "state")
	token = filepath.Join(run, "operator.token")
	secrets = filepath.Join(root, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		token:                             "operator-token-value",
		filepath.Join(secrets, "key"):     "secret-value",
		filepath.Join(state, "tether.db"): "database-bytes",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return state, token, secrets
}

func TestControlPlaneHidden(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	state, token, secrets := hiddenLayout(t, catalog, run)

	hidden, err := svc.controlPlaneHidden()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{state, token, secrets}; !slices.Equal(hidden, want) {
		t.Fatalf("hidden = %q, want %q", hidden, want)
	}

	// The run directory is hidden as a file only: the socket beside the token
	// must stay reachable.
	if slices.Contains(hidden, run) {
		t.Errorf("the run directory was hidden whole: %q", hidden)
	}

	// What does not exist is left out: a mount cannot cover nothing.
	if err := os.RemoveAll(secrets); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if hidden, err = svc.controlPlaneHidden(); err != nil || !slices.Equal(hidden, []string{state}) {
		t.Fatalf("hidden = %q, %v; want the state directory alone", hidden, err)
	}
}

func TestApplyControlPlaneProtection_SetsHiddenPaths(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	state, token, secrets := hiddenLayout(t, catalog, run)

	opts := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{state, token, secrets}; !slices.Equal(opts.HiddenPaths, want) {
		t.Fatalf("HiddenPaths = %q, want %q", opts.HiddenPaths, want)
	}

	// An agent that would work in a hidden directory is refused.
	for _, dir := range []string{secrets, state} {
		opts := agentsessions.StartOptions{Workdir: filepath.Join(dir, "work"), WorkspaceDir: t.TempDir()}
		err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts)
		if !errors.Is(err, launch.ErrLaunchInsideProtectedPath) {
			t.Errorf("work directory in %s: err = %v, want ErrLaunchInsideProtectedPath", dir, err)
		}
		if opts.HiddenPaths != nil {
			t.Errorf("a refused launch still set HiddenPaths %q", opts.HiddenPaths)
		}
	}

	// Protection off or an API stub: nothing to hide from.
	svc.protectionStatus = func() ProtectionStatus { return ProtectionStatus{} }
	off := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &off); err != nil || off.HiddenPaths != nil {
		t.Fatalf("protection off: HiddenPaths = %q, err = %v", off.HiddenPaths, err)
	}
}

// fakeCodexReader stands in for `codex exec`: each turn tries to read the
// state database, the operator token and a secret, and reports whether each read
// returned its content, plus whether the socket beside the token is still
// listed, as its agent message.
const fakeCodexReader = `#!/bin/sh
try() { if [ -n "$(cat "$1" 2>/dev/null)" ]; then echo read; else echo hidden; fi; }
sock=gone; [ -e %q ] && sock=listed
msg="state=$(try %q) token=$(try %q) secret=$(try %q) sock=$sock"
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"$msg\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// A wrapped agent cannot read the hidden paths while the daemon (this test
// process, outside the sandbox) can, and the socket in run/ stays reachable.
// It needs a bubblewrap that can build a namespace and skips without one, as
// CI does.
func TestProtectedLaunch_AgentCannotReadControlPlane(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	state, token, secrets := hiddenLayout(t, catalog, run)
	sock := filepath.Join(run, "tetherd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	script := fmt.Sprintf(fakeCodexReader, sock, filepath.Join(state, "tether.db"), token, filepath.Join(secrets, "key"))
	sessID, ws := startFakeCodex(t, svc, script, "--sandbox", "danger-full-access")

	if err := svc.SendTurn(context.Background(), sessID, "read everything"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "state=")
	if !strings.Contains(logData, "state=hidden token=hidden secret=hidden sock=listed") {
		t.Fatalf("agent reads = %q; want state, token and secret hidden and the socket still listed", logData)
	}
	for path, want := range map[string]string{
		filepath.Join(state, "tether.db"): "database-bytes",
		token:                             "operator-token-value",
		filepath.Join(secrets, "key"):     "secret-value",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("the daemon can no longer read %s: %q, %v", path, got, err)
		}
	}
}
