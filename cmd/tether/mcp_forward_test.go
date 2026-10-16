package main

import (
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardDaemonCredentialNeverUsesOperatorFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	t.Setenv("TETHER_MCP_TOKEN", "")
	savedCatalog, savedFile, savedSession := catalogPath, tokenFilePath, mcpSession
	t.Cleanup(func() { catalogPath, tokenFilePath, mcpSession = savedCatalog, savedFile, savedSession })
	catalogPath = filepath.Join(home, ".tether", "catalog")
	tokenFilePath = ""
	mcpSession = ""
	operatorToken, _ := identity.NewToken()
	envToken, _ := identity.NewToken()
	fileToken, _ := identity.NewToken()
	dir := filepath.Join(home, ".tether", "run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.token"), []byte(operatorToken), 0600); err != nil {
		t.Fatal(err)
	}
	token, err := forwardDaemonCredential()
	if err != nil || token != "" {
		t.Fatal("operator fallback entered thin route", err)
	}
	t.Setenv("TETHER_TOKEN", envToken)
	token, err = forwardDaemonCredential()
	if err != nil || token != envToken {
		t.Fatal("session env ignored", err)
	}
	file := filepath.Join(home, "explicit.token")
	if err := os.WriteFile(file, []byte(fileToken), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFilePath = file
	token, err = forwardDaemonCredential()
	if err != nil || token != fileToken {
		t.Fatal("explicit token file ignored", err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := forwardDaemonCredential(); err == nil {
		t.Fatal("unsafe explicit file fell through to env")
	}
}

// The worker's SSH --daemon-only route owns its upstream connections. Merely
// appending --forward-daemon cannot turn those calls into hub observations.
func TestForwardDaemonRejectsLegacyWorkerRoute(t *testing.T) {
	savedOnly, savedScopes := mcpDaemonOnly, mcpScopes
	t.Cleanup(func() { mcpDaemonOnly, mcpScopes = savedOnly, savedScopes })
	mcpDaemonOnly = true
	mcpScopes = "session.write"
	if err := runMCPForwardDaemon(&cobra.Command{}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("legacy worker route accepted: %v", err)
	}
}
