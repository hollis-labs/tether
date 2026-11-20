package mcptransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSavedGrantNarrowsOnlyAfterDigestAndIdentityValidation(t *testing.T) {
	for _, kind := range []string{"session", "agent"} {
		t.Run(kind, func(t *testing.T) {
			cat := &config.Catalog{MCPServerEnabled: map[string]bool{"keep": true, "disabled": false, "new": true}}
			floor := &mcpgateway.Profile{ReadOnly: true, Tools: mcpgateway.ToolRules{Deny: []string{"*_write"}}}
			saved := mcpgateway.SessionPolicy{SessionID: "s", AgentID: "a", Servers: []string{"keep", "removed", "disabled", "tether"}, ToolProfile: floor}.Seal()
			policy := saved
			notices := 0
			r := CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }, Session: func(context.Context, string) (mcpgateway.SessionPolicy, error) { return policy, nil }, OnNarrow: func(_ context.Context, p mcpgateway.SessionPolicy, issues []config.MCPGrantIssue) {
				notices++
				if p.Digest != saved.Digest || (notices == 1 && len(issues) != 2) {
					t.Fatalf("notice=%+v %+v", p, issues)
				}
			}}
			p := identity.Principal{Verified: true, ID: identity.SessionPrincipalIDPrefix + "s", Kind: kind, SessionID: "s"}
			if kind == "agent" {
				p.TokenID = "issued"
			}
			ctx := identity.WithPrincipal(context.Background(), p)
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			caller, err := r.Resolve(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(caller.Policy.Servers, []string{"keep", "tether"}) || caller.SavedDigest != saved.Digest || caller.Policy.ToolProfile == nil || !caller.Policy.ToolProfile.ReadOnly || notices != 1 {
				t.Fatalf("caller=%+v notices=%d", caller, notices)
			}
			if err := caller.Policy.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"session=s", "server=removed", "server=disabled", "reason=removed", "reason=disabled"} {
				if !strings.Contains(logs.String(), text) {
					t.Fatalf("missing %s in %s", text, logs.String())
				}
			}
			beforeOpts, err := ViewOptions(Caller{Policy: saved, SavedDigest: saved.Digest}, mcpgateway.Config{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			afterOpts, err := ViewOptions(caller, mcpgateway.Config{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := viewFingerprint(Caller{Principal: p, Policy: saved, SavedDigest: saved.Digest}, "token", beforeOpts)
			after, _ := viewFingerprint(caller, "token", afterOpts)
			if before != after {
				t.Fatal("safe catalog narrowing invalidates an open transport")
			}
			policy.Digest = "corrupt"
			if _, err := r.Resolve(ctx); !errors.Is(err, mcpgateway.ErrInvalidSessionMCPPolicy) {
				t.Fatalf("bad digest: %v", err)
			}
			policy = saved
			policy.SessionID = "other"
			policy = policy.Seal()
			if _, err := r.Resolve(ctx); !errors.Is(err, mcpgateway.ErrSessionMCPIdentity) {
				t.Fatalf("identity: %v", err)
			}
			if notices != 1 {
				t.Fatal("invalid snapshot caused a narrowing notice")
			}
			policy = saved
			cat.MCPServerEnabled = map[string]bool{}
			caller, err = r.Resolve(ctx)
			if err != nil || !slices.Equal(caller.Policy.Servers, []string{"tether"}) {
				t.Fatalf("all externals gone: %+v %v", caller, err)
			}
		})
	}
}

func TestAdmissionPolicyFailureCodesAndHints(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{mcpgateway.ErrInvalidSessionMCPPolicy, "mcp_policy_invalid"},
		{mcpgateway.ErrSessionMCPIdentity, "mcp_identity_mismatch"},
		{config.ErrInvalidMCPGrant, "mcp_grant_stale"},
	} {
		w := httptest.NewRecorder()
		admissionError(w, tc.err)
		if w.Code != 403 || !strings.Contains(w.Body.String(), tc.code) || !strings.Contains(w.Body.String(), "resume or relaunch") {
			t.Fatalf("%s: %d %s", tc.code, w.Code, w.Body.String())
		}
	}
}

func TestTransportSavedGrantRemovalKeepsEstablishedTetherConnection(t *testing.T) {
	f := newTransportFixture(t, true, false)
	ctx := context.Background()
	f.catMu.Lock()
	f.cat.MCPServerEnabled = map[string]bool{"removed": true, "disabled": true, "keep": true, "tether": false}
	f.cat.Global.MCP.Profiles["saved"] = mcpgateway.Profile{Servers: []string{"removed", "disabled", "keep", "tether"}}
	f.catMu.Unlock()
	if err := f.db.CreateSession(store.SessionRow{ID: "s", LogicalAgentID: "agent", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	profileID := "saved"
	floor := mcpgateway.Profile{Servers: []string{"removed", "disabled", "keep", "tether"}}
	if err := f.db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: "s", AgentID: "agent", Servers: []string{"removed", "disabled", "keep", "tether"}, Profile: &profileID, LaunchProfile: &floor, ToolProfile: &floor}.Seal()); err != nil {
		t.Fatal(err)
	}
	token, err := f.ids.Mint(ctx, identity.Principal{ID: "session:s", Kind: "session", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.New(f.addr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	f.catMu.Lock()
	delete(f.cat.MCPServerEnabled, "removed")
	f.cat.MCPServerEnabled["disabled"] = false
	f.catMu.Unlock()
	// A fresh view must also accept stale origins in saved and selected profiles.
	reconnect, err := client.New(f.addr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	if err != nil {
		t.Fatal("stale profile blocked reconnect", err)
	}
	defer func() { _ = reconnect.Close() }()
	recovery, err := reconnect.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_docs_list", Arguments: map[string]any{}})
	if err != nil || recovery.IsError {
		t.Fatalf("reconnected recovery: %+v %v", recovery, err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal("narrowing disconnected established session", err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "tether_whoami" {
			found = true
		}
	}
	if !found {
		t.Fatal("Tether recovery tool absent")
	}
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_docs_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("Tether call after removal: %+v %v", result, err)
	}
	f.catMu.Lock()
	delete(f.cat.MCPServerEnabled, "keep")
	f.catMu.Unlock()
	result, err = session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_docs_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("Tether call with no external servers: %+v %v", result, err)
	}
}

func TestStaleProfileOriginValidationPreservesAuthorityFloors(t *testing.T) {
	for _, servers := range [][]string{nil, {}, {"removed"}, {"removed", "keep", "tether"}} {
		t.Run(fmt.Sprintf("%#v", servers), func(t *testing.T) {
			profile := mcpgateway.Profile{Servers: servers, ReadOnly: true}
			id := "saved"
			saved := mcpgateway.SessionPolicy{SessionID: "s", Servers: []string{"keep", "tether"}, Profile: &id, LaunchProfile: &profile, ToolProfile: &profile}.Seal()
			caller := Caller{Policy: saved, Narrowed: []config.MCPGrantIssue{{Server: "removed", Reason: "removed"}}}
			opts, err := ViewOptions(caller, mcpgateway.Config{Profiles: map[string]mcpgateway.Profile{id: profile}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := selectedUpstreamOrigins(admission{catalog: &config.Catalog{MCPServerEnabled: map[string]bool{"keep": true, "tether": false}}, caller: caller, options: opts})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{}
			if servers == nil || slices.Contains(servers, "keep") {
				want = []string{"keep"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("origins=%v want=%v", got, want)
			}
			for _, floor := range opts.AuthorityProfiles {
				if !floor.Profile.ReadOnly || (floor.Profile.Servers == nil) != (servers == nil) || !slices.Equal(floor.Profile.Servers, servers) {
					t.Fatalf("floor changed: %+v", floor)
				}
			}
			if (opts.Profile.Profile.Servers == nil) != (servers == nil) {
				t.Fatal("selected nil/empty changed")
			}
			caller.Narrowed = nil
			if servers != nil && slices.Contains(servers, "removed") {
				if _, err := selectedUpstreamOrigins(admission{catalog: &config.Catalog{MCPServerEnabled: map[string]bool{"keep": true}}, caller: caller, options: opts}); err == nil {
					t.Fatal("unrelated unknown origin accepted")
				}
			}
		})
	}
}
