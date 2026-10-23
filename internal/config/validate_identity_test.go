package config

import (
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestValidateAgentURNAndMCPScopes(t *testing.T) {
	good := "msg://agent/agent-mux/agt_454f2timi7"
	cases := []struct {
		name   string
		agents map[string]Agent
		want   string
	}{
		{"valid", map[string]Agent{"pm": {ID: "pm", URN: good, MCPScopes: []string{"message.write", "registry.write"}}}, ""},
		{"not an agent urn", map[string]Agent{"pm": {ID: "pm", URN: "msg://user/local/operator"}}, "invalid urn"},
		{"garbage", map[string]Agent{"pm": {ID: "pm", URN: "agt_454f2timi7"}}, "invalid urn"},
		// An agent instance address (a session id as the fourth segment) is derived from a running session, never declared (CW-20261023-0018).
		{"instance address", map[string]Agent{"pm": {ID: "pm", URN: good + "/0b7f6a1e-0000-4000-8000-000000000001"}}, "invalid urn"},
		{"duplicate", map[string]Agent{"a": {ID: "a", URN: good}, "b": {ID: "b", URN: good}}, "same urn"},
		{"unknown scope", map[string]Agent{"pm": {ID: "pm", MCPScopes: []string{"root"}}}, "unknown mcp_scopes"},
		{"editable agents", map[string]Agent{"aom": {ID: "aom", EditableAgents: []string{"chat"}}, "chat": {ID: "chat"}}, ""},
		{"editable agent not in catalog", map[string]Agent{"aom": {ID: "aom", EditableAgents: []string{"ghost"}}}, "not a catalog agent"},
		{"editable agent twice", map[string]Agent{"aom": {ID: "aom", EditableAgents: []string{"chat", "chat"}}, "chat": {ID: "chat"}}, "twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Catalog{Agents: tc.agents}).Validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDefaultMCPScopesAreValid(t *testing.T) {
	for _, s := range launchprofile.DefaultMCPScopes {
		if !launchprofile.ValidMCPScope(s) {
			t.Fatalf("default scope %q not valid", s)
		}
	}
}

func TestValidateManagerRejectsBypass(t *testing.T) {
	mgr := func(mode string) Agent {
		return Agent{ID: "mgr", Roles: []string{launchprofile.RoleManager}, Permissions: launchprofile.AgentPermissions{PermissionMode: mode}}
	}
	bypassGlobal := Global{}
	bypassGlobal.Catalog.Defaults.PermissionMode = PermissionModeBypass
	cases := []struct {
		name   string
		global Global
		agent  Agent
		want   string
	}{
		{"explicit bypass", Global{}, mgr(PermissionModeBypass), "blanket bypass"},
		{"inherited global bypass", bypassGlobal, mgr(""), "blanket bypass"},
		{"explicit default over global bypass", bypassGlobal, mgr(PermissionModeDefault), ""},
		{"unset", Global{}, mgr(""), ""},
		{"worker may bypass", Global{}, Agent{ID: "mgr", Roles: []string{"worker"}, Permissions: launchprofile.AgentPermissions{PermissionMode: PermissionModeBypass}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Catalog{Global: tc.global, Agents: map[string]Agent{"mgr": tc.agent}}).Validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
