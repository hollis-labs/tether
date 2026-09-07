package e2e

// harness_test.go — proves the e2e harness itself works: a real `mux
// daemon run` OS process, spawned against an isolated state root,
// answers a real health check over its real UDS socket, and never
// touches anything under the real user's home directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureDaemon_StartsIsolatedAndAnswersHealth(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	d := StartFixtureDaemon(t)

	if strings.HasPrefix(d.StateRoot, home) {
		t.Fatalf("fixture state root %q is under the real home directory %q -- must be fully isolated", d.StateRoot, home)
	}
	if !strings.HasPrefix(d.SocketAddr, "unix:"+d.StateRoot) {
		t.Fatalf("socket addr %q is not inside the fixture state root %q", d.SocketAddr, d.StateRoot)
	}

	// Confirm the auto-seeded global.yaml really did rewrite every
	// ~/.tether reference -- the single most important safety property
	// of this whole package.
	globalYAML, err := os.ReadFile(filepath.Join(d.CatalogDir, "global.yaml"))
	if err != nil {
		t.Fatalf("read seeded global.yaml: %v", err)
	}
	if strings.Contains(string(globalYAML), "~/.tether") || strings.Contains(string(globalYAML), home) {
		t.Fatalf("seeded global.yaml still references the real home/state root:\n%s", globalYAML)
	}

	if err := d.Client().Ping(t.Context()); err != nil {
		t.Fatalf("Ping real daemon over real socket: %v", err)
	}

	// The state DB must exist at the isolated path once the daemon has
	// actually booted (proves store.Open + migrations ran for real, in
	// the real binary, against the real fixture path).
	if _, err := os.Stat(d.StateDBPath()); err != nil {
		t.Fatalf("state db not created at expected isolated path %s: %v", d.StateDBPath(), err)
	}
}

func TestFixtureDaemon_KillAndRestart_SocketComesBackHealthy(t *testing.T) {
	d := StartFixtureDaemon(t)
	if err := d.Client().Ping(t.Context()); err != nil {
		t.Fatalf("initial ping: %v", err)
	}

	d.Kill()
	if err := d.Client().Ping(t.Context()); err == nil {
		t.Fatal("expected Ping to fail immediately after a real SIGKILL")
	}

	d.Restart()
	if err := d.Client().Ping(t.Context()); err != nil {
		t.Fatalf("ping after restart: %v", err)
	}
}
