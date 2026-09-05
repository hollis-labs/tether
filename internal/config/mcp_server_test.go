package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMCPServers(t *testing.T) {
	t.Run("valid stdio entry", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "hadron.yaml"), `
id: hadron
transport: stdio
command: /usr/local/bin/hadrond
args: [mcp]
env:
  HADRON_TOKEN: "static-token"
scopes: [run.write]
tags: [automation]
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		e := entries[0]
		if e.ID != "hadron" {
			t.Errorf("ID = %q, want %q", e.ID, "hadron")
		}
		if e.Transport != "stdio" {
			t.Errorf("Transport = %q, want %q", e.Transport, "stdio")
		}
		if e.Command != "/usr/local/bin/hadrond" {
			t.Errorf("Command = %q", e.Command)
		}
		if len(e.Args) != 1 || e.Args[0] != "mcp" {
			t.Errorf("Args = %v", e.Args)
		}
		if e.Env["HADRON_TOKEN"] != "static-token" {
			t.Errorf("Env[HADRON_TOKEN] = %q", e.Env["HADRON_TOKEN"])
		}
	})

	t.Run("valid sse entry", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "vanta.yaml"), `
id: vanta
transport: sse
url: "http://localhost:8090/mcp/sse"
tags: [memory]
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].URL != "http://localhost:8090/mcp/sse" {
			t.Errorf("URL = %q", entries[0].URL)
		}
	})

	t.Run("env var expansion", func(t *testing.T) {
		t.Setenv("TEST_MUX_TOKEN", "secret-tok")
		t.Setenv("TEST_MUX_URL", "http://host:9090/sse")
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "test.yaml"), `
id: test-server
transport: sse
url: "${TEST_MUX_URL}"
token: "${TEST_MUX_TOKEN}"
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Token != "secret-tok" {
			t.Errorf("Token = %q, want %q", entries[0].Token, "secret-tok")
		}
		if entries[0].URL != "http://host:9090/sse" {
			t.Errorf("URL = %q, want %q", entries[0].URL, "http://host:9090/sse")
		}
	})

	t.Run("disabled entry excluded", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "off.yaml"), `
id: off-server
transport: stdio
command: /bin/off
enabled: false
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected 0 entries (disabled), got %d", len(entries))
		}
	})

	t.Run("catalog loader includes disabled entries", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "off.yaml"), `
id: off-server
transport: stdio
command: /bin/off
enabled: false
`)
		entries, err := LoadMCPServerCatalog(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].IsEnabled() {
			t.Error("expected disabled entry")
		}
	})

	t.Run("missing enabled defaults to true", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "on.yaml"), `
id: on-server
transport: stdio
command: /bin/on
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
	})

	t.Run("parse error", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "bad.yaml"), `{invalid yaml: [`)
		_, err := LoadMCPServers(dir)
		if err == nil {
			t.Error("expected parse error, got nil")
		}
	})

	t.Run("missing directory is not an error", func(t *testing.T) {
		dir := t.TempDir() // no mcp-servers subdir
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Errorf("expected nil error for missing dir, got %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected 0 entries, got %d", len(entries))
		}
	})
}

// write creates a file (and parent dirs) with the given content.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stubResolver records the references it is asked for and answers from a map.
type stubResolver struct {
	values map[string]string
	err    error
	seen   []string
}

func (s *stubResolver) Resolve(_ context.Context, ref string) (string, error) {
	s.seen = append(s.seen, ref)
	if s.err != nil {
		return "", s.err
	}
	v, ok := s.values[ref]
	if !ok {
		return "", fmt.Errorf("no such secret %q", ref)
	}
	return v, nil
}

// withResolver swaps the package resolver for the duration of a subtest.
func withResolver(t *testing.T, r secretRefResolver) {
	t.Helper()
	prev := secretResolver
	secretResolver = r
	t.Cleanup(func() { secretResolver = prev })
}

func TestLoadMCPServersResolvesSecretRefs(t *testing.T) {
	t.Run("env, token and args refs resolve", func(t *testing.T) {
		stub := &stubResolver{values: map[string]string{
			"keychain://openai/work":                       "resolved-openai-key",
			"keychain://tesseract/mcp-token":               "resolved-bearer",
			"helper://torque-apikey-helper/torque/default": "resolved-helper",
		}}
		withResolver(t, stub)

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), `
id: tesseract
transport: stdio
command: /usr/local/bin/tesseract
args: [mcp, --token, "keychain://tesseract/mcp-token"]
token: "helper://torque-apikey-helper/torque/default"
env:
  OPENAI_API_KEY: "keychain://openai/work"
  PLAIN: "not-a-ref"
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		e := entries[0]
		if e.Env["OPENAI_API_KEY"] != "resolved-openai-key" {
			t.Errorf("Env[OPENAI_API_KEY] = %q, want resolved value", e.Env["OPENAI_API_KEY"])
		}
		if e.Env["PLAIN"] != "not-a-ref" {
			t.Errorf("Env[PLAIN] = %q, want literal passthrough", e.Env["PLAIN"])
		}
		if len(e.Args) != 3 || e.Args[2] != "resolved-bearer" {
			t.Errorf("Args = %v, want the token arg resolved", e.Args)
		}
		if e.Token != "resolved-helper" {
			t.Errorf("Token = %q, want resolved value", e.Token)
		}
	})

	t.Run("catalog loader leaves refs unresolved", func(t *testing.T) {
		stub := &stubResolver{values: map[string]string{"keychain://openai/work": "resolved-openai-key"}}
		withResolver(t, stub)

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), `
id: tesseract
transport: stdio
command: /usr/local/bin/tesseract
env:
  OPENAI_API_KEY: "keychain://openai/work"
`)
		entries, err := LoadMCPServerCatalog(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if got := entries[0].Env["OPENAI_API_KEY"]; got != "keychain://openai/work" {
			t.Errorf("Env[OPENAI_API_KEY] = %q, want the unresolved reference", got)
		}
		if len(stub.seen) != 0 {
			t.Errorf("catalog loader resolved %v; it must never touch the resolver", stub.seen)
		}
	})

	t.Run("unresolvable ref is a hard error naming the server and field", func(t *testing.T) {
		withResolver(t, &stubResolver{err: errors.New("keychain entry not found")})

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), `
id: tesseract
transport: stdio
command: /usr/local/bin/tesseract
env:
  OPENAI_API_KEY: "keychain://openai/work"
`)
		_, err := LoadMCPServers(dir)
		if err == nil {
			t.Fatal("expected an error for an unresolvable reference, got nil")
		}
		for _, want := range []string{"tesseract", "env.OPENAI_API_KEY", "keychain://openai/work"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("resolved secret never appears in the error", func(t *testing.T) {
		const secret = "sk-super-secret-value"
		withResolver(t, &stubResolver{
			values: map[string]string{"keychain://openai/work": secret},
			err:    errors.New("helper failed"),
		})

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "tesseract.yaml"), `
id: tesseract
transport: stdio
command: /usr/local/bin/tesseract
env:
  OPENAI_API_KEY: "keychain://openai/work"
`)
		_, err := LoadMCPServers(dir)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked the secret material: %q", err)
		}
	})

	t.Run("disabled entries are never resolved", func(t *testing.T) {
		stub := &stubResolver{values: map[string]string{"keychain://openai/work": "resolved"}}
		withResolver(t, stub)

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "off.yaml"), `
id: off-server
transport: stdio
command: /bin/off
enabled: false
env:
  OPENAI_API_KEY: "keychain://openai/work"
`)
		if _, err := LoadMCPServers(dir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(stub.seen) != 0 {
			t.Errorf("resolved %v for a disabled entry", stub.seen)
		}
	})

	t.Run("catalogs without refs never invoke the resolver", func(t *testing.T) {
		stub := &stubResolver{}
		withResolver(t, stub)

		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "hadron.yaml"), `
id: hadron
transport: stdio
command: /usr/local/bin/hadrond
args: [mcp]
env:
  HADRON_TOKEN: "static-token"
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entries[0].Env["HADRON_TOKEN"] != "static-token" {
			t.Errorf("literal value changed: %q", entries[0].Env["HADRON_TOKEN"])
		}
		if len(stub.seen) != 0 {
			t.Errorf("resolver invoked for a literal-only catalog: %v", stub.seen)
		}
	})

	t.Run("env expansion still applies to args", func(t *testing.T) {
		t.Setenv("TEST_MCP_ARG", "expanded-arg")
		dir := t.TempDir()
		write(t, filepath.Join(dir, "mcp-servers", "test.yaml"), `
id: test-server
transport: stdio
command: /bin/test
args: [mcp, "${TEST_MCP_ARG}"]
`)
		entries, err := LoadMCPServers(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries[0].Args) != 2 || entries[0].Args[1] != "expanded-arg" {
			t.Errorf("Args = %v, want ${VAR} expanded", entries[0].Args)
		}
	})
}
