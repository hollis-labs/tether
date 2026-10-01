package main

import (
	"strings"
	"testing"

	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/tether/internal/config"
)

// tether doctor fails when an agent names an undefined sandbox profile: the
// daemon starts, but refuses that agent's launches (CW-20261001-0130).
func TestCheckSandboxProfiles(t *testing.T) {
	profiles := map[string]sandbox.Profile{"workspace-only": {ID: "workspace-only"}}
	good := &config.Catalog{
		Agents:          map[string]config.Agent{"a": {ID: "a", Permissions: config.AgentPermissions{DefaultSandbox: "workspace-only"}}, "b": {ID: "b"}},
		SandboxProfiles: profiles,
	}
	if r := checkSandboxProfiles(good); r.Status != statusOK {
		t.Fatalf("good catalog: %+v; want ok", r)
	}
	bad := &config.Catalog{
		Agents:          map[string]config.Agent{"a": {ID: "a", Permissions: config.AgentPermissions{DefaultSandbox: "gone"}}},
		SandboxProfiles: profiles,
	}
	r := checkSandboxProfiles(bad)
	if r.Status != statusFail || !strings.Contains(r.Message, `"gone"`) || r.Remedy == "" {
		t.Fatalf("bad catalog: %+v; want a failure naming the profile, with a remedy", r)
	}
}
