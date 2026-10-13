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
		{"duplicate", map[string]Agent{"a": {ID: "a", URN: good}, "b": {ID: "b", URN: good}}, "same urn"},
		{"unknown scope", map[string]Agent{"pm": {ID: "pm", MCPScopes: []string{"root"}}}, "unknown mcp_scopes"},
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
