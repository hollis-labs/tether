package mcptransport

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func TestCallerResolver_CredentialDeterminesGrant(t *testing.T) {
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"one": true, "two": true, "disabled": false}}
	cat.Global.Identity.MCPGrants = map[string]config.PrincipalMCPGrant{"svc": {Servers: []string{"one"}}, "bad": {Servers: []string{"typo"}}}
	snapshot := mcpgateway.SessionPolicy{SessionID: "s1", AgentID: "agent-a", Servers: []string{"two"}}.Seal()
	r := CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }, Session: func(_ context.Context, id string) (mcpgateway.SessionPolicy, error) {
		if id != "s1" {
			return mcpgateway.SessionPolicy{}, errors.New("unknown session")
		}
		return snapshot, nil
	}}
	for _, tc := range []struct {
		name    string
		p       identity.Principal
		count   int
		agent   string
		wantErr bool
	}{
		{"session", identity.Principal{ID: "session-p", Kind: "session", SessionID: "s1"}, 1, "agent-a", false},
		{"operator", identity.Principal{ID: identity.OperatorID, Kind: "operator"}, 2, "", false},
		{"service", identity.Principal{ID: "svc", Kind: "service", Scopes: []string{"*"}}, 1, "", false},
		{"unmapped interactive", identity.Principal{ID: "interactive", Kind: "interactive", Scopes: []string{"*"}}, 0, "", false},
		{"bad grant", identity.Principal{ID: "bad", Kind: "service"}, 0, "", true},
		{"session without snapshot", identity.Principal{ID: "old", Kind: "session", SessionID: "old"}, 0, "", true},
		{"nonoperator cannot claim operator", identity.Principal{ID: "svc", Kind: "operator"}, 0, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller, err := r.Resolve(identity.WithPrincipal(context.Background(), tc.p))
			if (err != nil) != tc.wantErr {
				t.Fatalf("caller=%+v err=%v", caller, err)
			}
			if tc.wantErr {
				return
			}
			if len(caller.Policy.Servers) != tc.count || caller.Policy.Servers == nil || caller.Policy.AgentID != tc.agent {
				t.Fatalf("caller=%+v", caller)
			}
			if len(caller.Policy.Servers) > 0 {
				caller.Policy.Servers[0] = "mutated"
			}
		})
	}
	if snapshot.Servers[0] != "two" || cat.Global.Identity.MCPGrants["svc"].Servers[0] != "one" {
		t.Fatal("view aliases stored authority")
	}
	if _, err := r.Resolve(context.Background()); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("unverified caller accepted", err)
	}
}

func TestCallerResolver_RefusesChangedOrMismatchedSessionPolicy(t *testing.T) {
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"one": true}}
	policy := mcpgateway.SessionPolicy{SessionID: "other", Servers: []string{"one"}}.Seal()
	r := CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }, Session: func(context.Context, string) (mcpgateway.SessionPolicy, error) { return policy, nil }}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "p", Kind: "session", SessionID: "s"})
	if _, err := r.Resolve(ctx); err == nil {
		t.Fatal("cross-session snapshot accepted")
	}
	policy.SessionID = "s" // changing a grant without resealing is invalid
	if _, err := r.Resolve(ctx); err == nil {
		t.Fatal("changed digest accepted")
	}
	policy = policy.Seal()
	cat.MCPServerEnabled["one"] = false
	if _, err := r.Resolve(ctx); !errors.Is(err, config.ErrInvalidMCPGrant) {
		t.Fatal("disabled grant accepted", err)
	}
}
