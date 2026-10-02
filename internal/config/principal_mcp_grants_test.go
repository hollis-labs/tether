package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrincipalMCPGrants_LoadAndValidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"global.yaml":               "identity:\n  mcp_grants:\n    'msg://agent/local/interactive':\n      servers: [valid]\n    service-bad:\n      servers: [typo]\n    empty: {}\n    disabled:\n      servers: [disabled]\n",
		"mcp-servers/valid.yaml":    "id: valid\ntransport: http\nurl: https://private.invalid/secret\ntoken: helper://must-not-resolve\n",
		"mcp-servers/disabled.yaml": "id: disabled\nenabled: false\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := Load(root)
	if err != nil {
		t.Fatalf("shared loader must permit repair: %v", err)
	}
	if err := cat.ValidateMCPGrants(); !errors.Is(err, ErrInvalidMCPGrant) || !strings.Contains(err.Error(), "service-bad") || !strings.Contains(err.Error(), "disabled") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "helper://") {
		t.Fatalf("whole validation=%v", err)
	}
	servers, err := cat.PrincipalMCPServers("msg://agent/local/interactive")
	if err != nil || len(servers) != 1 || servers[0] != "valid" {
		t.Fatalf("unrelated bad grant blocked valid principal: %v %v", servers, err)
	}
	servers[0] = "changed"
	again, _ := cat.PrincipalMCPServers("msg://agent/local/interactive")
	if again[0] != "valid" {
		t.Fatal("grant aliases mutable catalog slice")
	}
	for _, id := range []string{"unknown", "empty"} {
		servers, err := cat.PrincipalMCPServers(id)
		if err != nil || servers == nil || len(servers) != 0 {
			t.Fatalf("%s must grant explicit zero: %#v %v", id, servers, err)
		}
	}
	if _, err := cat.PrincipalMCPServers("service-bad"); !errors.Is(err, ErrInvalidMCPGrant) {
		t.Fatal(err)
	}
}
