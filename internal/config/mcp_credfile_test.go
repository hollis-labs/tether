//go:build unix

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/credfile"
)

const fileCredSecret = "file-sourced-credential-0123456789"

// credFile writes a credential with the given mode and returns its path. The
// chmod follows the write so the umask cannot change the mode under test.
func credFile(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(fileCredSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMCPServers_FileCredentialRefs(t *testing.T) {
	// Isolate home so the test does not depend on the runner's own.
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("env, token, url and args refs are read from the file", func(t *testing.T) {
		dir := t.TempDir()
		cred := credFile(t, filepath.Join(home, ".tether", "secrets"), "tesseract.token", 0o600)
		write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), `
id: tesseract
transport: stdio
command: /usr/local/bin/tesseract
args: [mcp, --token, "file://`+cred+`", --plain, literal]
token: "file://`+cred+`"
env:
  TESSERACT_TOKEN: "file://`+cred+`"
  PLAIN: not-a-ref
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("LoadMCPServers: %v", err)
		}
		e := entries[0]
		if e.Env["TESSERACT_TOKEN"] != fileCredSecret || e.Token != fileCredSecret {
			t.Fatalf("env/token not resolved: env=%q token=%q", e.Env["TESSERACT_TOKEN"], e.Token)
		}
		if e.Env["PLAIN"] != "not-a-ref" {
			t.Errorf("a literal env value changed: %q", e.Env["PLAIN"])
		}
		if len(e.Args) != 5 || e.Args[2] != fileCredSecret || e.Args[4] != "literal" {
			t.Fatalf("args = %q, want the token arg resolved and the rest untouched", e.Args)
		}

		// The file-sourced argument is redaction material, and the literal ones are not.
		redactions := e.ArgumentRedactionValues()
		if !slices.Contains(redactions, fileCredSecret) {
			t.Fatalf("ArgumentRedactionValues() = %q, want the file-sourced value", redactions)
		}
		for _, unwanted := range []string{"mcp", "--token", "literal", "--plain"} {
			if slices.Contains(redactions, unwanted) {
				t.Errorf("ArgumentRedactionValues() contains the literal argument %q", unwanted)
			}
		}
	})

	t.Run("the catalog listing leaves the reference unresolved", func(t *testing.T) {
		dir := t.TempDir()
		cred := credFile(t, filepath.Join(home, "secrets"), "tok", 0o600)
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nenv:\n  K: \"file://"+cred+"\"\n")
		entries, err := LoadMCPServerCatalog(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := entries[0].Env["K"]; got != "file://"+cred {
			t.Fatalf("catalog listing resolved the reference: Env[K] = %q", got)
		}
	})

	t.Run("a ~/ path is the operator's home", func(t *testing.T) {
		dir := t.TempDir()
		credFile(t, filepath.Join(home, ".tether", "secrets"), "hadrond.token", 0o600)
		write(t, filepath.Join(dir, "mcp-servers", "hadron.yaml"), "id: hadron\ntransport: stdio\ncommand: /bin/h\nenv:\n  T: \"file://~/.tether/secrets/hadrond.token\"\n")
		entries, err := LoadMCPServers(dir)
		if err != nil || entries[0].Env["T"] != fileCredSecret {
			t.Fatalf("got %v, %v", entries, err)
		}
	})

	t.Run("a group- or world-readable file is refused, naming the server, field and file", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o640, 0o644} {
			dir := t.TempDir()
			cred := credFile(t, filepath.Join(home, "loose"), "tok", mode)
			write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), "id: tesseract\ntransport: stdio\ncommand: /bin/t\nenv:\n  T: \"file://"+cred+"\"\n")
			_, err := LoadMCPServers(dir)
			if !errors.Is(err, credfile.ErrTooPermissive) {
				t.Fatalf("mode %04o: err = %v, want ErrTooPermissive", mode, err)
			}
			for _, want := range []string{`mcp server "tesseract"`, "env.T", cred} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("mode %04o: error %q does not name %q", mode, err, want)
				}
			}
			if strings.Contains(err.Error(), fileCredSecret) {
				t.Fatalf("error exposes the credential: %v", err)
			}
		}
	})

	t.Run("a missing file is an error, not an empty credential", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(home, "no-such-dir", "tok")
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nargs: [--token, \"file://"+missing+"\"]\n")
		_, err := LoadMCPServers(dir)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
		if !strings.Contains(err.Error(), `mcp server "x"`) || !strings.Contains(err.Error(), "args[1]") || !strings.Contains(err.Error(), missing) {
			t.Fatalf("error should name the server, the argument and the file: %v", err)
		}
	})

	t.Run("a relative path is refused", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nenv:\n  T: \"file://secrets/token\"\n")
		if _, err := LoadMCPServers(dir); !errors.Is(err, credfile.ErrNotAbsolute) {
			t.Fatalf("err = %v, want ErrNotAbsolute", err)
		}
	})

	t.Run("a symlink in the catalog may not lead outside the catalog and home", func(t *testing.T) {
		dir := t.TempDir()
		outside := credFile(t, t.TempDir(), "elsewhere-token", 0o600)
		link := filepath.Join(dir, "mcp-servers", "token-link")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nenv:\n  T: \"file://"+link+"\"\n")
		_, err := LoadMCPServers(dir)
		if !errors.Is(err, credfile.ErrSymlinkEscape) {
			t.Fatalf("err = %v, want ErrSymlinkEscape", err)
		}
		if strings.Contains(err.Error(), fileCredSecret) {
			t.Fatalf("error exposes the credential: %v", err)
		}
	})

	t.Run("a symlink that leads into the catalog or home is followed", func(t *testing.T) {
		dir := t.TempDir()
		target := credFile(t, filepath.Join(home, "vault"), "tok", 0o600)
		link := filepath.Join(dir, "mcp-servers", "token-link")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nenv:\n  T: \"file://"+link+"\"\n")
		entries, err := LoadMCPServers(dir)
		if err != nil || entries[0].Env["T"] != fileCredSecret {
			t.Fatalf("got %v, %v", entries, err)
		}
	})

	t.Run("a disabled entry's file is not read", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "off.yaml"), "id: off\nenabled: false\ntransport: stdio\ncommand: /bin/x\nenv:\n  T: \"file://"+filepath.Join(home, "absent")+"\"\n")
		entries, err := LoadMCPServers(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("got %v, %v", entries, err)
		}
	})

	t.Run("another keychain or literal value is unaffected", func(t *testing.T) {
		withResolver(t, &stubResolver{values: map[string]string{"keychain://a/b": "kc-value"}})
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "x.yaml"), "id: x\ntransport: stdio\ncommand: /bin/x\nenv:\n  K: \"keychain://a/b\"\n  L: plain\n")
		entries, err := LoadMCPServers(dir)
		if err != nil || entries[0].Env["K"] != "kc-value" || entries[0].Env["L"] != "plain" {
			t.Fatalf("got %v, %v", entries, err)
		}
	})
}
