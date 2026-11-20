package mcptransport

import (
	"bytes"
	"context"
	"errors"
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
	f.cat.MCPServerEnabled = map[string]bool{"removed": true, "disabled": true, "keep": true}
	f.catMu.Unlock()
	if err := f.db.CreateSession(store.SessionRow{ID: "s", LogicalAgentID: "agent", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: "s", AgentID: "agent", Servers: []string{"removed", "disabled", "keep"}}.Seal()); err != nil {
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
