package mcptransport

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
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
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "p", Kind: "session", SessionID: "s"})
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

// An issued session-bound agent token (#365) binds to its session like the
// session credential does; nothing else that is merely token-shaped does.
func TestCallerResolver_BoundAgentTokenBindsToItsSession(t *testing.T) {
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"one": true, "two": true}}
	policy := mcpgateway.SessionPolicy{SessionID: "s1", AgentID: "agent-a", Servers: []string{"two"}}.Seal()
	r := CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }, Session: func(_ context.Context, id string) (mcpgateway.SessionPolicy, error) {
		if id != "s1" {
			return mcpgateway.SessionPolicy{}, errors.New("unknown session")
		}
		return policy, nil
	}}
	agent := identity.Principal{Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1", TokenID: "jti-1", Scopes: []string{"tether.messages"}}
	caller, err := r.Resolve(identity.WithPrincipal(context.Background(), agent))
	if err != nil {
		t.Fatal(err)
	}
	if caller.Policy.AgentID != "agent-a" || len(caller.Policy.Servers) != 1 || caller.Policy.Servers[0] != "two" {
		t.Fatalf("agent token did not get its session's policy: %+v", caller.Policy)
	}
	if caller.Principal.TokenID != "jti-1" || caller.MachineAccess != "" {
		t.Fatalf("principal %+v machine access %q", caller.Principal, caller.MachineAccess)
	}
	for name, p := range map[string]identity.Principal{
		"no token id":              {Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1"},
		"no session id":            {Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", TokenID: "j"},
		"subject names another":    {Verified: true, ID: identity.SessionPrincipalIDPrefix + "other", Kind: "agent", SessionID: "s1", TokenID: "j"},
		"unverified":               {ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1", TokenID: "j"},
		"session that has no row":  {Verified: true, ID: identity.SessionPrincipalIDPrefix + "gone", Kind: "agent", SessionID: "gone", TokenID: "j"},
		"operator-minted narrow":   {Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "j", Scopes: []string{"tether.read"}},
		"operator-minted with sid": {Verified: true, ID: identity.OperatorID, Kind: "operator", SessionID: "s1", TokenID: "j"},
		"device-kind with a sid":   {Verified: true, ID: "msg://device/d", Kind: "device", SessionID: "s1", TokenID: "j"},
	} {
		if _, err := r.Resolve(identity.WithPrincipal(context.Background(), p)); err == nil {
			t.Errorf("%s: resolved a gateway caller", name)
		}
	}
}

// A service or interactive principal carries read-write machine access only
// through an mcp_grants entry bound to a machine_authority read-write agent.
// The operator credential and unbound principals stay read-only.
func TestCallerResolver_MachineAccessFromGrantAgent(t *testing.T) {
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"cerberus": true}}
	cat.Agents = map[string]config.LaunchProfile{
		"rw":        {MachineAuthority: true, MachineAccess: launch.MachineAccessReadWrite},
		"ro":        {MachineAuthority: true, MachineAccess: launch.MachineAccessReadOnly},
		"unflagged": {MachineAccess: launch.MachineAccessReadWrite},
	}
	servers := []string{"cerberus"}
	cat.Global.Identity.MCPGrants = map[string]config.PrincipalMCPGrant{
		"svc-rw":        {Servers: servers, Agent: "rw"},
		"svc-ro":        {Servers: servers, Agent: "ro"},
		"svc-unflagged": {Servers: servers, Agent: "unflagged"},
		"svc-unknown":   {Servers: servers, Agent: "gone"},
		"svc-unbound":   {Servers: servers},
		"int-rw":        {Servers: servers, Agent: "rw"},
	}
	r := CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }}
	for _, tc := range []struct {
		p    identity.Principal
		want string
	}{
		{identity.Principal{ID: "svc-rw", Kind: "service"}, launch.MachineAccessReadWrite},
		{identity.Principal{ID: "int-rw", Kind: "interactive"}, launch.MachineAccessReadWrite},
		{identity.Principal{ID: "svc-ro", Kind: "service"}, ""},
		{identity.Principal{ID: "svc-unflagged", Kind: "service"}, ""},
		{identity.Principal{ID: "svc-unknown", Kind: "service"}, ""},
		{identity.Principal{ID: "svc-unbound", Kind: "service"}, ""},
		{identity.Principal{ID: "svc-other", Kind: "service"}, ""},
		{identity.Principal{ID: identity.OperatorID, Kind: "operator"}, ""},
	} {
		t.Run(tc.p.ID, func(t *testing.T) {
			caller, err := r.Resolve(identity.WithPrincipal(context.Background(), tc.p))
			if err != nil || caller.MachineAccess != tc.want || caller.Policy.AgentID != "" {
				t.Fatalf("caller=%+v err=%v", caller, err)
			}
		})
	}
}
