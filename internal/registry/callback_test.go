package registry_test

// callback_test.go — coverage for the Sync callback resolvers (T-v060-01-04).
//
// Matrix:
//   - FileResolver: happy path, missing file:// prefix, path outside catalog,
//     symlink-escape, missing-file errors.Is(os.ErrNotExist), oversize file,
//     context cancellation.
//   - CLIResolver: happy path, missing cli:// prefix, empty command, non-zero
//     exit code, timeout, oversize stdout.
//
// All tests use t.TempDir() for fixture roots; no test mutates ~/.tether/.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// ─── FileResolver ────────────────────────────────────────────────────────────

func TestFileResolver_HappyPath(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "agent.yaml")
	body := []byte("display_name: Alpha\nrole: implementer\n")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	got, err := r.Resolve(context.Background(), "file://"+target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("payload = %q, want %q", string(got), string(body))
	}
}

func TestFileResolver_MissingFilePrefix(t *testing.T) {
	root := canonTempDir(t)
	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	_, err = r.Resolve(context.Background(), "/etc/passwd")
	if !errors.Is(err, registry.ErrPayloadInvalid) {
		t.Errorf("err = %v, want ErrPayloadInvalid", err)
	}
}

func TestFileResolver_PathOutsideRoot(t *testing.T) {
	root := canonTempDir(t)
	outside := canonTempDir(t)
	target := filepath.Join(outside, "stranger.yaml")
	if err := os.WriteFile(target, []byte("x: 1"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	_, err = r.Resolve(context.Background(), "file://"+target)
	if !errors.Is(err, registry.ErrPathOutsideRoot) {
		t.Errorf("err = %v, want ErrPathOutsideRoot", err)
	}
}

func TestFileResolver_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows; v1 resolver targets unix")
	}
	root := canonTempDir(t)
	outside := canonTempDir(t)
	realTarget := filepath.Join(outside, "secret.yaml")
	if err := os.WriteFile(realTarget, []byte("oauth_token: leak"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	link := filepath.Join(root, "escape.yaml")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	_, err = r.Resolve(context.Background(), "file://"+link)
	if !errors.Is(err, registry.ErrPathOutsideRoot) {
		t.Errorf("err = %v, want ErrPathOutsideRoot (symlink escape)", err)
	}
}

func TestFileResolver_NotExistSurfaces(t *testing.T) {
	root := canonTempDir(t)
	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	missing := filepath.Join(root, "nope.yaml")
	_, err = r.Resolve(context.Background(), "file://"+missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestFileResolver_PayloadTooLarge(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "big.yaml")
	// Tiny resolver max (overridden via test hook); fixture is 4 KiB.
	body := make([]byte, 4096)
	for i := range body {
		body[i] = 'x'
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	registry.SetFileResolverMaxBytesForTest(r, 1024)

	_, err = r.Resolve(context.Background(), "file://"+target)
	if !errors.Is(err, registry.ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestFileResolver_ContextCanceled(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "agent.yaml")
	if err := os.WriteFile(target, []byte("display_name: A\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	r, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Resolve(ctx, "file://"+target)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFileResolver_EmptyRootRejected(t *testing.T) {
	if _, err := registry.NewFileResolver(""); err == nil {
		t.Error("NewFileResolver(\"\") returned nil err, want non-nil")
	}
}

// ─── CLIResolver ─────────────────────────────────────────────────────────────

func TestCLIResolver_HappyPath(t *testing.T) {
	r := registry.NewCLIResolver()
	got, err := r.Resolve(context.Background(), "cli://echo hello")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "hello\n"
	if string(got) != want {
		t.Errorf("payload = %q, want %q", string(got), want)
	}
}

func TestCLIResolver_MissingCLIPrefix(t *testing.T) {
	r := registry.NewCLIResolver()
	_, err := r.Resolve(context.Background(), "echo hello")
	if !errors.Is(err, registry.ErrPayloadInvalid) {
		t.Errorf("err = %v, want ErrPayloadInvalid", err)
	}
}

func TestCLIResolver_EmptyCommand(t *testing.T) {
	r := registry.NewCLIResolver()
	_, err := r.Resolve(context.Background(), "cli://")
	if !errors.Is(err, registry.ErrPayloadInvalid) {
		t.Errorf("err = %v, want ErrPayloadInvalid", err)
	}
}

func TestCLIResolver_NonZeroExit(t *testing.T) {
	r := registry.NewCLIResolver()
	// `false` is a POSIX standard that exits with code 1 — the simplest
	// non-zero-exit fixture. strings.Fields-style arg splitting keeps the
	// command as a single token, no quoting needed.
	_, err := r.Resolve(context.Background(), "cli://false")
	if err == nil {
		t.Fatal("err = nil, want non-zero exit error")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("err = %v, want message mentioning exit code", err)
	}
}

func TestCLIResolver_Timeout(t *testing.T) {
	r := registry.NewCLIResolver()
	registry.SetCLIResolverTimeoutForTest(r, 100*time.Millisecond)

	start := time.Now()
	_, err := r.Resolve(context.Background(), "cli://sleep 5")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err = nil, want timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %s; expected timeout to kill child much sooner", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want message mentioning timed out", err)
	}
}

func TestCLIResolver_PayloadTooLarge(t *testing.T) {
	r := registry.NewCLIResolver()
	registry.SetCLIResolverMaxBytesForTest(r, 64)
	// Use printf to produce a deterministic >64 byte payload via a single
	// argv token after strings.Fields splits "printf xxxxxxx...".
	body := strings.Repeat("x", 256)
	_, err := r.Resolve(context.Background(), "cli://printf "+body)
	if !errors.Is(err, registry.ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// canonTempDir returns a t.TempDir() canonicalized via EvalSymlinks. On
// macOS, t.TempDir() returns a /var/folders/... path that is itself a
// symlink to /private/var/folders/..., which breaks naive prefix checks.
// Canonicalize once so test fixtures and the resolver share one form.
func canonTempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if canon, err := filepath.EvalSymlinks(d); err == nil {
		return canon
	}
	return d
}
