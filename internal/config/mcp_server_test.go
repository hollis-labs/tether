package config

import (
	"os"
	"path/filepath"
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
