package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedGatewayView_AllNativeToolsKeepHiddenOriginsPrivate(t *testing.T) {
	const hidden, secret = "ungranted-canary-origin", "SUPERSECRETKEY"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer remote.Close()
	remote.Close() // force an HTTP dial error that includes the credential URL
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}, {ID: hidden, Transport: "sse", URL: remote.URL + "/?api_key=" + secret, AllowUnconfinedRemote: true}}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, status := range r.pool.StatusSummary() {
		if strings.Contains(status.Error, secret) {
			t.Fatal("shared pool retained URL credential")
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "native.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "restricted", Kind: "service"})
			a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{}, Store: db}, nil)
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.NewGatewayView(ctx, a, ProxyOptions{ServerFilter: []string{"alpha"}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: string(mode), Source: "test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			cs := connectDaemonView(ctx, t, v)
			check := func(value any) {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				for _, canary := range []string{hidden, secret} {
					if strings.Contains(string(raw), canary) {
						t.Fatal("restricted native output exposed hidden origin/credential")
					}
				}
			}
			listed, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(listed)
			// Audit every native registration through real SDK dispatch. Mutators
			// have zero scopes and cannot change the disposable service or host.
			for _, entry := range v.Service.Snapshot().Entries {
				if entry.Origin != "tether" {
					continue
				}
				name, args := entry.Tool.Name, map[string]any{}
				if mode == mcpgateway.Search {
					name, args = "tether_tool_call", map[string]any{"name": entry.Tool.Name, "arguments": args}
				}
				result, callErr := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
				check(result)
				if callErr != nil {
					check(callErr.Error())
				}
			}
			for _, name := range listToolNames(t, cs) {
				if !isGatewayTool(name) {
					continue
				}
				result, callErr := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: map[string]any{}})
				check(result)
				if callErr != nil {
					check(callErr.Error())
				}
			}
		})
	}
}

func connectDaemonView(ctx context.Context, t *testing.T, v *GatewayView) *mcpsdk.ClientSession {
	t.Helper()
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := v.Server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "daemon-view-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestSharedGatewayView_EmptyUpstreamGrantKeepsNativeTools(t *testing.T) {
	r, err := NewSharedUpstreams(nil, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "session-p", Kind: "session", SessionID: "s", Scopes: []string{"message.write"}})
	a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.NewGatewayView(ctx, a, ProxyOptions{ServerFilter: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	cs := connectDaemonView(ctx, t, v)
	if !slices.Contains(listToolNames(t, cs), "tether_catalog_list_projects") {
		t.Fatal("native tools disappeared with zero upstreams")
	}
	result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_catalog_list_projects", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("native read failed: %+v %v", result, err)
	}
	result, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_session_launch", Arguments: map[string]any{"session_id": "not-real"}})
	if err == nil && !result.IsError {
		t.Fatal("verified scopes were bypassed by native dispatch")
	}
	v.Close()
	if _, err := cs.ListTools(ctx, nil); err == nil {
		t.Fatal("view Close left protocol session live")
	}
}

func TestSharedGatewayView_ProfileAndGrantApplyToEveryPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "one", Transport: "http"}, {ID: "two", Transport: "http"}}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Controlled local SDK upstreams isolate the view policy from process setup,
	// which TestSharedUpstreams_ConfinedOneProcessForConcurrentViews verifies.
	for _, origin := range []string{"one", "two"} {
		s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: origin, Version: "test"}, nil)
		name := origin + "_read"
		s.AddTool(&mcpsdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		})
		s.AddTool(&mcpsdk.Tool{Name: origin + "_write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			t.Error("excluded write dispatched")
			return nil, nil
		})
		s.AddTool(&mcpsdk.Tool{Name: origin + "_private", InputSchema: map[string]any{"type": "object"}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			t.Error("launch-profile denied tool dispatched")
			return nil, nil
		})
		st, ct := mcpsdk.NewInMemoryTransports()
		ss, err := s.Connect(context.Background(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ss.Close() })
		cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "fixture", Version: "test"}, nil).Connect(context.Background(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		tools, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		r.pool.mu.Lock()
		r.pool.statuses[origin].client = cs
		r.pool.mu.Unlock()
		if _, err := r.pool.publish(context.Background(), origin, cs, tools.Tools, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "caller", Kind: "session", SessionID: "actual", Scopes: []string{"message.write"}})
			a, err := NewVerifiedAdapter(ctx, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			m := string(mode)
			profile := mcpgateway.Profile{Servers: []string{"one", "two"}, Instructions: "Requested full profile."}
			floor := mcpgateway.Profile{Servers: []string{"one"}, ReadOnly: true, Tools: mcpgateway.ToolRules{Deny: []string{"*_private"}}}
			v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"one", "two"}, Profile: mcpgateway.ProfileSelection{ID: "full", Profile: &profile}, AuthorityProfiles: []mcpgateway.ProfileSelection{{ID: "launch-readonly", Profile: &floor}}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: m, Source: "test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			cs := connectDaemonView(ctx, t, v)
			if cs.InitializeResult().Instructions != profile.Instructions {
				t.Fatal("profile instructions lost")
			}
			names := listToolNames(t, cs)
			if mode == mcpgateway.Flat && (!slices.Contains(names, "one_read") || slices.Contains(names, "one_write") || slices.Contains(names, "two_read")) {
				t.Fatal(names)
			}
			for _, name := range []string{"one_read", "one_write", "one_private", "two_read"} {
				callName := name
				args := map[string]any{}
				if mode == mcpgateway.Search {
					callName = "tether_tool_call"
					args = map[string]any{"name": name, "arguments": map[string]any{}}
				}
				result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: callName, Arguments: args})
				if name == "one_read" {
					if err != nil || result.IsError {
						t.Fatalf("permitted call failed %v %v", result, err)
					}
				} else if err == nil && !result.IsError {
					t.Fatal("excluded call succeeded", name)
				}
			}
			for _, origin := range v.Service.Snapshot().Origins {
				if origin.ID == "two" {
					t.Fatal("hidden origin status leaked")
				}
			}
		})
	}
}
