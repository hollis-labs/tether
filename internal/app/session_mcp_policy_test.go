package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestSessionMCPPolicy_CapturesResolvedSelectorsWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		value *string
		want  []string
	}{{nil, launch.DefaultMCPServers}, {new(string), []string{}}, {func() *string { v := "one,two"; return &v }(), []string{"one", "two"}}} {
		plan := &launch.Plan{Env: map[string]string{"TETHER_TOKEN": "must-not-store", "UPSTREAM_SECRET": "also-secret", "TETHER_MCP_PROFILE": "narrow", "TETHER_MCP_DISCOVERY_MODE": "search"}}
		if tc.value != nil {
			plan.Env["TETHER_MCP_SERVERS"] = *tc.value
		}
		p := sessionMCPPolicy("session", "agent", plan)
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.Servers, tc.want) || p.AgentID != "agent" || p.SessionID != "session" || *p.Profile != "narrow" || *p.DiscoveryMode != "search" {
			t.Fatalf("snapshot=%+v", p)
		}
		raw, _ := json.Marshal(p)
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "must-not-store") || strings.Contains(string(raw), "TETHER_TOKEN") {
			t.Fatal("secret/environment persisted")
		}
	}
}

func TestSessionMCPPolicy_PersistedBeforeCredentialDelivery(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	p, err := svc.Store.SessionMCPPolicy(context.Background(), id)
	if err != nil || p.AgentID != "agent" || p.SessionID != id {
		t.Fatalf("delivered credential without bound policy: %+v %v", p, err)
	}
}
