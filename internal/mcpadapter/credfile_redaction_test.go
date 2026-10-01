//go:build unix

package mcpadapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-mcp/supervise"
	"github.com/hollis-labs/tether/internal/config"
)

// Distinct values for the environment and the argument, so each path's redaction
// is proven on its own rather than masked by the other.
const (
	fileCredentialSecret    = "file-sourced-env-secret-9876543210"
	fileCredentialArgSecret = "file-sourced-arg-secret-0123456789"
)

// loadFileCredentialEntry loads a stdio upstream whose token comes from a 0600
// credential file, once through env and once through an argument (CW-20261001-0229).
func loadFileCredentialEntry(t *testing.T, command string) []config.MCPServerEntry {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := filepath.Join(home, "token")
	argCred := filepath.Join(home, "argtoken")
	for path, value := range map[string]string{cred: fileCredentialSecret, argCred: fileCredentialArgSecret} {
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := t.TempDir()
	if err := os.MkdirAll(filepath.Join(catalog, "mcp-servers"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "id: filecred\ntransport: stdio\ncommand: " + command + "\nargs: [mcp, \"file://" + argCred + "\"]\nenv:\n  UPSTREAM_TOKEN: \"file://" + cred + "\"\n"
	if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", "filecred.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServers(catalog)
	if err != nil {
		t.Fatalf("LoadMCPServers: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	return entries
}

// A credential read from a file is scrubbed from an upstream's stderr tail
// (which is kept in server status and exit records) whether it reached the
// child through its environment or through an argument, and from tool-call
// error text across every server.
func TestFileCredential_IsRedactedFromUpstreamStderrAndToolErrors(t *testing.T) {
	script := filepath.Join(t.TempDir(), "upstream.sh")
	body := "#!/bin/sh\necho \"env=$UPSTREAM_TOKEN arg=$2 ok\" >&2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	entries := loadFileCredentialEntry(t, script)
	entry := entries[0]
	if entry.Env["UPSTREAM_TOKEN"] != fileCredentialSecret || entry.Args[1] != fileCredentialArgSecret {
		t.Fatalf("env/args not resolved from their files: %q %q", entry.Env["UPSTREAM_TOKEN"], entry.Args)
	}

	// The child really receives the value, and what it writes to stderr is scrubbed.
	u, _, err := spawnStdioUpstream(entry)
	if err != nil {
		t.Fatalf("spawnStdioUpstream: %v", err)
	}
	if waitErr := u.cmd.Wait(); waitErr != nil {
		t.Fatalf("upstream exited with %v", waitErr)
	}
	tail := u.stderr.String()
	if strings.Contains(tail, fileCredentialSecret) || strings.Contains(tail, fileCredentialArgSecret) {
		t.Fatalf("stderr tail exposes a file-sourced credential: %q", tail)
	}
	if !strings.Contains(tail, "env=[redacted] arg=[redacted] ok") {
		t.Fatalf("stderr tail = %q; want the child's echo with both occurrences scrubbed (so the value did reach the child)", tail)
	}

	// The same values scrub tool-call error text for every configured server.
	both := "env " + fileCredentialSecret + " arg " + fileCredentialArgSecret
	if got := proxyRedactionSet(entries).Redact(both); got != "env [redacted] arg [redacted]" {
		t.Fatalf("proxy redaction set = %q", got)
	}
	if got := supervise.Redact(both, stderrRedactionValues(entry)); got != "env [redacted] arg [redacted]" {
		t.Fatalf("stderrRedactionValues = %q", got)
	}
}

// A launch observation records the command path. If a path embeds the
// credential it is suppressed rather than recorded.
func TestFileCredential_LaunchObservationSuppressesAnEmbeddedValue(t *testing.T) {
	entries := loadFileCredentialEntry(t, "/bin/true")
	entry := entries[0]
	for _, secret := range []string{fileCredentialSecret, fileCredentialArgSecret} {
		entry.Command = "/opt/tools/" + secret + "/upstream"
		obs := observeLaunch(exec.Command(entry.Command), entry)
		if strings.Contains(obs.Selector, secret) || strings.Contains(obs.ResolvedPath, secret) {
			t.Fatalf("launch observation exposes the credential: %+v", obs)
		}
		if obs.Resolution != "redacted" {
			t.Fatalf("Resolution = %q, want redacted", obs.Resolution)
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for the pool's goroutines to log into.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An sse/http upstream whose url comes from a credential file: a failed connect
// names the endpoint, so the raw error carries the secret. It must not reach the
// "upstream unavailable" log line or ServerStatus.Error, which mux_health and the
// sysop API return (CW-20261001-0229 review).
func TestFileCredentialURL_IsRedactedFromStatusAndLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const secretPath = "url-path-secret-4242424242"
	secretURL := "http://127.0.0.1:1/" + secretPath // port 1: connection refused
	cred := filepath.Join(home, "url")
	if err := os.WriteFile(cred, []byte(secretURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := t.TempDir()
	if err := os.MkdirAll(filepath.Join(catalog, "mcp-servers"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "id: remote\ntransport: http\nurl: \"file://" + cred + "\"\n"
	if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", "remote.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServers(catalog)
	if err != nil {
		t.Fatalf("LoadMCPServers: %v", err)
	}
	if entries[0].URL != secretURL {
		t.Fatalf("url not resolved from the file: %q", entries[0].URL)
	}

	// Precondition: the raw connect error does carry the secret. Without this the
	// test would pass whether or not anything is redacted.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probe := NewClientPool(entries, NewToolRegistry())
	_, rawErr := probe.connect(ctx, entries[0])
	if rawErr == nil || !strings.Contains(rawErr.Error(), secretPath) {
		t.Fatalf("precondition: the raw connect error should name the endpoint; got %v", rawErr)
	}

	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pool := NewClientPool(entries, NewToolRegistry())
	if err := pool.Start(ctx); err != nil {
		t.Logf("Start: %v", err)
	}
	t.Cleanup(pool.Shutdown)
	st := awaitStatus(t, pool, "remote", func(s ServerStatus) bool { return s.Status == "failed" })

	if strings.Contains(st.Error, secretPath) {
		t.Fatalf("ServerStatus.Error exposes the url secret: %q", st.Error)
	}
	if !strings.Contains(st.Error, "[redacted]") {
		t.Fatalf("ServerStatus.Error = %q; want the endpoint scrubbed, not dropped", st.Error)
	}
	if out := logs.String(); strings.Contains(out, secretPath) {
		t.Fatalf("the log exposes the url secret: %s", out)
	} else if !strings.Contains(out, "upstream unavailable") {
		t.Fatalf("expected the failure to be logged: %s", out)
	}
}

// Redaction changes the text, not the cause: callers that test the error with
// errors.Is still see through it.
func TestRedactUpstreamError_KeepsTheCause(t *testing.T) {
	entry := config.MCPServerEntry{ID: "x", Token: "tok-secret-value"}
	cause := errors.New("sentinel")
	got := redactUpstreamError(errors.Join(cause, errors.New("auth tok-secret-value rejected")), entry)
	if strings.Contains(got.Error(), "tok-secret-value") {
		t.Fatalf("not redacted: %q", got)
	}
	if !errors.Is(got, cause) {
		t.Fatal("the cause was lost")
	}
	// Nothing to scrub: the error comes back as it was, not rewrapped.
	plain := errors.New("connection refused")
	untouched := func(label string, got error) {
		t.Helper()
		var rewrapped *redactedError
		if errors.As(got, &rewrapped) || !errors.Is(got, plain) || got.Error() != plain.Error() {
			t.Fatalf("%s: an error with nothing to scrub was rewritten: %v", label, got)
		}
	}
	untouched("no credential in the text", redactUpstreamError(plain, entry))
	// An entry with no credentials at all has nothing to scrub either.
	untouched("empty credential values", redactUpstreamError(plain, config.MCPServerEntry{ID: "y"}))
	if redactUpstreamError(nil, entry) != nil {
		t.Fatal("nil must stay nil")
	}
}
