package mcpadapter

// whoami_tools_test.go — end-to-end coverage for tether_whoami (T08).
// Reuses registry_tools_test.go's daemon-routed harness
// (newRegistryAdapter, callRegistryTool -- both already register
// bindings tools too, and registerWhoamiTools needs the same treatment).

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestWhoamiTool_UnregisteredActor_NoErrorEmptyFields(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_whoami", map[string]any{"as": "msg://session/agent-mux/sess_never_registered"})
	if res.IsError {
		t.Fatalf("whoami: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	whoami, _ := body["whoami"].(map[string]any)
	if whoami["profile"] != nil || whoami["binding"] != nil {
		t.Fatalf("expected no profile/binding for an unregistered actor, got %+v", whoami)
	}
}

func TestWhoamiTool_RegisteredActor_PopulatesProfile(t *testing.T) {
	a, svc := newRegistryAdapter(t)
	p, err := svc.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "Worker", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	res := callRegistryTool(t, a, "tether_whoami", map[string]any{"as": p.URN})
	if res.IsError {
		t.Fatalf("whoami: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	whoami, _ := body["whoami"].(map[string]any)
	profile, _ := whoami["profile"].(map[string]any)
	if profile["urn"] != p.URN {
		t.Fatalf("whoami profile urn = %v, want %v", profile["urn"], p.URN)
	}
}

func TestWhoamiTool_RequiresAs(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_whoami", map[string]any{})
	if !res.IsError {
		t.Fatal("expected an error result with no as")
	}
	if code := parseToolJSON(t, res)["code"]; code != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", code)
	}
}
