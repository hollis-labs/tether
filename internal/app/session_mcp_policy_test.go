package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
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
		p, err := sessionMCPPolicy("session", "agent", plan, mcpgateway.Config{Profiles: map[string]mcpgateway.Profile{"narrow": {ReadOnly: true}}})
		if err != nil {
			t.Fatal(err)
		}
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

func TestSessionMCPPolicy_InvalidSelectorFailsSession(t *testing.T) {
	for _, entry := range []struct{ key, value string }{{"TETHER_MCP_PROFILE", ""}, {"TETHER_MCP_DISCOVERY_MODE", "typo"}} {
		t.Run(entry.key, func(t *testing.T) {
			svc, runtime, id, _ := credentialLaunch(t)
			plan, err := svc.Store.GetLaunchPlan(id)
			if err != nil {
				t.Fatal(err)
			}
			plan.Env[entry.key] = entry.value
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Store.DB().Exec("UPDATE launch_plans SET plan_json=? WHERE session_id=?", string(raw), id); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.LaunchSession(id); !errors.Is(err, mcpgateway.ErrInvalidSessionMCPPolicy) {
				t.Fatal("missing typed policy error", err)
			}
			row, err := svc.Store.GetSession(id)
			if err != nil || row.State != "failed" {
				t.Fatalf("invalid policy left session in %v: %v", row, err)
			}
			if len(runtime.options.Env) != 0 {
				t.Fatal("invalid policy reached runtime")
			}
		})
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
