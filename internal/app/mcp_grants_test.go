package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestAgentOpsMCPGrantValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, profile, override string
		invalid, empty          bool
	}{
		{name: "unknown boot grant", profile: "mcp_servers: [missing]\n", invalid: true},
		{name: "disabled boot grant", profile: "mcp_servers: [disabled]\n", invalid: true},
		{name: "unknown override", override: `{"env":{"TETHER_MCP_SERVERS":"missing"}}`, invalid: true},
		{name: "empty boot grant", profile: "mcp_servers: []\n", empty: true},
		{name: "empty override", override: `{"env":{"TETHER_MCP_SERVERS":""}}`, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			svc := buildTestService(t, map[string]config.Agent{"test-agent": {ID: "test-agent"}}, root)
			svc.CatalogRoot = ""
			svc.Catalog.MCPServerEnabled = map[string]bool{"torque": true, "disabled": false}
			plan := basePlan()
			plan.Env[launch.MCPServersEnv] = "torque"
			input := CreateSessionInput{LaunchID: "test-launch", Override: tc.override}
			if tc.profile != "" {
				input.BootProfileFile = filepath.Join(root, "profile.yaml")
				if err := os.WriteFile(input.BootProfileFile, []byte("id: test\n"+tc.profile), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := svc.applyAgentOps(plan, input)
			if tc.invalid {
				if !errors.Is(err, config.ErrInvalidMCPGrant) {
					t.Fatalf("error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if value, set := plan.Env[launch.MCPServersEnv]; tc.empty && (!set || value != "" || len(launch.EffectiveMCPServers(plan.Env)) != 0) {
				t.Fatalf("empty grant lost: %v", plan.Env)
			}
		})
	}
}

func TestMCPGrantChecksCurrentEnablement(t *testing.T) {
	root := t.TempDir()
	svc := buildTestService(t, map[string]config.Agent{"test-agent": {ID: "test-agent"}}, root)
	svc.Catalog.MCPServerEnabled = map[string]bool{"torque": true}
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "torque.yaml"), []byte("id: torque\nenabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := basePlan()
	plan.Env[launch.MCPServersEnv] = "torque"
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); !errors.Is(err, config.ErrInvalidMCPGrant) {
		t.Fatalf("must use current enablement: %v", err)
	}
}

func TestMCPGrantEnabledRemoteIsValid(t *testing.T) {
	root := t.TempDir()
	svc := buildTestService(t, map[string]config.Agent{"test-agent": {ID: "test-agent"}}, root)
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	// No allow_unconfined_remote opt-in: a protected Codex proxy will exclude
	// it with a visible status, but it still exists and is enabled in the catalog.
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "tangent.yaml"), []byte("id: tangent\ntransport: http\nurl: https://must-not-contact.example/mcp\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := basePlan()
	plan.Env[launch.MCPServersEnv] = "tangent"
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatalf("catalog-enabled remote grant must remain valid: %v", err)
	}
}

func TestMCPGrantExpandsCatalogRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "catalog")
	if err := os.MkdirAll(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "torque.yaml"), []byte("id: torque\n"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{"test-agent": {ID: "test-agent"}}, "~/catalog")
	plan := basePlan()
	plan.Env[launch.MCPServersEnv] = "torque"
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatalf("catalog root accepted by config.Load must resolve here too: %v", err)
	}
}
