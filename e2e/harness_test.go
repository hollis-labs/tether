// Package e2e exercises the complete messaging-vnext implementation
// through REAL public daemon/client surfaces — a genuinely spawned `mux
// daemon run` OS process, driven via internal/client.Client over its
// real UDS socket — in isolated temporary state roots. This is T11
// (CW-20260906-0042)'s own scope text: "Exercise the complete
// implementation through real public daemon/client surfaces in isolated
// temporary environments." Every prior task's own test suite (T01-T10)
// deliberately used in-process httptest.NewServer/api.NewHandler by
// their own doc comments; this package is what T11 adds on top of that,
// not a replacement for it.
//
// # Isolation and safety
//
// Every FixtureDaemon gets its own fresh, isolated temp directory as the
// state root (the parent of --catalog). Because that directory's global.yaml does
// not yet exist on first start, internal/app.New's own
// maybeAutoSeedCatalog auto-seeds a minimal catalog with every
// "~/.tether" path in it rewritten to the fixture's own state root
// (internal/setup.WriteCatalog's StateRoot option) — listen_addr,
// pid_file and state_db all resolve inside the temp dir, never the real
// ~/.tether. No test in this package ever passes a bare "--catalog"
// pointing at $HOME, and nothing here ever touches a real provider
// credential or launches a real coding-agent process — matching the
// standing "no production agent launch, credentials, daemon reload, or
// DB mutation" constraint this task's own acceptance #3 restates.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// muxBinary builds cmd/mux exactly once per test process (subsequent
// FixtureDaemons reuse the same binary) and returns its path.
func muxBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tether-e2e-bin-")
		if err != nil {
			buildErr = fmt.Errorf("mkdir temp: %w", err)
			return
		}
		binPath = filepath.Join(dir, "mux")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/mux/")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build ./cmd/mux: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build mux binary: %v", buildErr)
	}
	return binPath
}

// repoRoot returns the module root. e2e/ is a direct child of it, and
// `go test` runs with the package directory as its working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repoRoot %q does not look like the module root (no go.mod): %v", root, err)
	}
	return root
}

// FixtureDaemon is one isolated, genuinely running `mux daemon run`
// process. Construct with StartFixtureDaemon.
type FixtureDaemon struct {
	t          *testing.T
	bin        string
	StateRoot  string // temp dir; parent of CatalogDir, run/, state/, logs/
	CatalogDir string
	SocketAddr string // "unix:<path>", suitable for client.New / daemon.DialHTTPClient
	cmd        *exec.Cmd
	logPath    string
}

// StartFixtureDaemon builds (once, shared across all fixtures in this
// test process) and spawns a real mux daemon process against a fresh,
// fully isolated state root. Blocks until the daemon answers a real
// GET /health over its real socket, or fails the test after a bounded
// timeout. Registers cleanup to terminate the process.
func StartFixtureDaemon(t *testing.T) *FixtureDaemon {
	t.Helper()
	// Deliberately NOT t.TempDir(): it nests the full test name into the
	// path (".../TestSomeVeryDescriptiveName12345/001"), which routinely
	// blows macOS's ~104-byte sockaddr_un limit for the unix socket this
	// state root will hold at <root>/run/muxd.sock ("bind: invalid
	// argument" is exactly that limit, not a real daemon bug). A short,
	// random, flat directory avoids it.
	stateRoot, err := os.MkdirTemp("", "te2e")
	if err != nil {
		t.Fatalf("mkdir isolated state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
	return startFixtureDaemonAt(t, stateRoot)
}

// startFixtureDaemonAt spawns a real daemon against an already-prepared
// state root (does NOT create or clean it up) -- for scenarios that need
// to seed real files (e.g. a legacy state.db) before the daemon's first
// boot, which StartFixtureDaemon's own fresh-tempdir-then-boot ordering
// can't support.
func startFixtureDaemonAt(t *testing.T, stateRoot string) *FixtureDaemon {
	t.Helper()
	d := &FixtureDaemon{
		t:          t,
		bin:        muxBinary(t),
		StateRoot:  stateRoot,
		CatalogDir: filepath.Join(stateRoot, "catalog"),
		SocketAddr: "unix:" + filepath.Join(stateRoot, "run", "muxd.sock"),
	}
	d.start()
	t.Cleanup(d.Stop)
	return d
}

// start spawns the daemon process and waits for it to become healthy.
// Both StartFixtureDaemon and Restart call this.
func (d *FixtureDaemon) start() {
	t := d.t
	d.logPath = filepath.Join(d.StateRoot, fmt.Sprintf("daemon-run-%d.log", time.Now().UnixNano()))
	logFile, err := os.Create(d.logPath) //nolint:gosec // G304: path constructed from t.TempDir(), not user input
	if err != nil {
		t.Fatalf("create daemon log %s: %v", d.logPath, err)
	}

	cmd := exec.Command(d.bin, "daemon", "run", "--catalog", d.CatalogDir) //nolint:gosec // G204: bin is our own just-built test binary
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start %s daemon run: %v", d.bin, err)
	}
	d.cmd = cmd

	if err := d.waitHealthy(20 * time.Second); err != nil {
		b, _ := os.ReadFile(d.logPath)
		t.Fatalf("fixture daemon did not become healthy: %v\n--- daemon log (%s) ---\n%s", err, d.logPath, b)
	}
}

func (d *FixtureDaemon) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c := d.Client()
	var lastErr error
	for time.Now().Before(deadline) {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lastErr = c.Ping(ctx)
		}()
		if lastErr == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

// Client returns a fresh internal/client.Client pointed at this
// fixture's real UDS socket -- the same typed client every mux CLI
// command and every production consumer uses, exercising the real
// public daemon surface end to end.
func (d *FixtureDaemon) Client() *client.Client {
	return client.New(d.SocketAddr)
}

// Kill sends SIGKILL to the daemon process -- simulates a genuine crash
// (not a graceful shutdown): whatever was durably on disk at the instant
// of the signal is exactly what a restart has to recover from. Blocks
// until the process has actually exited.
func (d *FixtureDaemon) Kill() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	_, _ = d.cmd.Process.Wait()
}

// Restart starts a fresh `mux daemon run` process against the exact same
// state root (same catalog, same state.db, same socket path) -- proving
// data survives a crash+restart cycle, not merely that a fresh process
// can be spawned. Safe to call after Kill or Stop.
func (d *FixtureDaemon) Restart() {
	d.start()
}

// Stop gracefully terminates the daemon (SIGTERM, matching real operator
// shutdown), falling back to SIGKILL if it doesn't exit promptly.
// Registered automatically as test cleanup by StartFixtureDaemon; safe to
// call again manually (e.g. before Restart) since it's a no-op once the
// process has already exited.
func (d *FixtureDaemon) Stop() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = d.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
	}
}

// StateDBPath is the fixture's real SQLite state file, for tests that
// need to seed it (upgrade-from-fixture-data drills) or inspect it
// directly after the daemon has stopped.
func (d *FixtureDaemon) StateDBPath() string {
	return filepath.Join(d.StateRoot, "state", "tether.db")
}
