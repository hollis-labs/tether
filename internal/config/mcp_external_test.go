package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalMCPServerLoadAndGrant(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "mcp-servers", "shared-services.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("id: shared-services\nserved_by: external\nserved_at: standalone gateway :18190\ntoken: file:///does-not-exist\n")
	cat, err := LoadLayered(root)
	if err != nil {
		t.Fatal(err)
	}
	if cat.MCPServerServedAt["shared-services"] != "standalone gateway :18190" {
		t.Fatal("external serving location missing")
	}
	if !cat.MCPServerExternal["shared-services"] {
		t.Fatal("external metadata missing")
	}
	if err := cat.ValidateMCPGrant("test", []string{"shared-services"}); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadMCPServers(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spawn loader: %v %v", entries, err)
	}
	entries, unknown, err := LoadMCPServersConfined(root, []string{"shared-services"})
	if err != nil || len(entries) != 0 || len(unknown) != 0 {
		t.Fatalf("confined: %v %v %v", entries, unknown, err)
	}
	checks, err := CheckMCPServerCredentialFiles(root)
	if err != nil || len(checks) != 0 {
		t.Fatalf("credentials: %v %v", checks, err)
	}
	write("id: shared-services\nserved_by: typo\n")
	if _, err := LoadMCPServerCatalog(root); err == nil {
		t.Fatal("invalid served_by accepted")
	}
	write("id: shared-services\nserved_by: external\nenabled: false\n")
	cat, err = LoadLayered(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.ValidateMCPGrant("test", []string{"shared-services"}); err == nil {
		t.Fatal("disabled external grant accepted")
	}
}
