//go:build unix

package mcpadapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
