package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const appResourceURI = "ui://fixture/card.html"

func resourceFixture(t *testing.T, name string, reads *atomic.Int32) *mcpsdk.ClientSession {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "test"}, &mcpsdk.ServerOptions{PageSize: 1})
	server.AddTool(&mcpsdk.Tool{Name: name + "_card", InputSchema: map[string]any{"type": "object"}, Meta: mcpsdk.Meta{"ui": map[string]any{"resourceUri": appResourceURI}}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{"fixture": "tool-result"}, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "card"}}}, nil
	})
	read := func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		reads.Add(1)
		raw, _ := json.Marshal(req.Params.Meta)
		if strings.Contains(string(raw), "forged") {
			t.Error("client-invented provenance forwarded")
		}

		if req.Params.Meta["fixture"] != "request" {
			t.Error("resource request metadata lost")
		}
		return &mcpsdk.ReadResourceResult{Meta: mcpsdk.Meta{"fixture": "read-result"}, Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/html;profile=mcp-app", Text: "<html><body>fixture</body></html>", Meta: mcpsdk.Meta{"ui": map[string]any{"prefersBorder": true}}}}}, nil
	}
	server.AddResource(&mcpsdk.Resource{URI: appResourceURI, Name: "card", MIMEType: "text/html;profile=mcp-app", Meta: mcpsdk.Meta{"fixture": "resource"}}, read)
	server.AddResource(&mcpsdk.Resource{URI: "fixture://private", Name: "private"}, read)
	server.AddResourceTemplate(&mcpsdk.ResourceTemplate{URITemplate: "ui://fixture/{name}.html", Name: "cards", MIMEType: "text/html;profile=mcp-app", Meta: mcpsdk.Meta{"fixture": "template"}}, read)
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "fixture-client", Version: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestSharedGatewayResources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "app", Transport: "http"}, {ID: "hidden", Transport: "http"}}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Real SDK sessions, without launching unrelated supervisors.
	var reads atomic.Int32
	upstream := resourceFixture(t, "app", &reads)
	tools, err := upstream.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.pool.statuses["app"] = &clientStatus{client: upstream, state: "connected", accepted: true}
	if _, err := r.pool.publish(context.Background(), "app", upstream, tools.Tools, false); err != nil {
		t.Fatal(err)
	}
	var hiddenReads atomic.Int32
	hidden := resourceFixture(t, "hidden", &hiddenReads)
	hiddenTools, err := hidden.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.pool.statuses["hidden"] = &clientStatus{client: hidden, state: "connected", accepted: true}
	if _, err := r.pool.publish(context.Background(), "hidden", hidden, hiddenTools.Tools, false); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"flat", "search"} {
		for _, restriction := range []string{"none", "deny", "allow", "floor", "grant", "collision"} {
			t.Run(mode+"/"+restriction, func(t *testing.T) {
				ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "resource-caller", Kind: "session", SessionID: "actual"})
				a, err := NewVerifiedAdapter(ctx, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				pub := &capturePub{}
				opts := ProxyOptions{Only: true, ServerFilter: []string{"app"}, Publisher: pub, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: mode, Source: "test"}}}}
				profile := mcpgateway.Profile{}
				switch restriction {
				case "deny":
					profile.Tools.Deny = []string{"app_card"}
				case "allow":
					profile.Tools.Allow = []string{"app_card"}
				case "floor":
					opts.AuthorityProfiles = []mcpgateway.ProfileSelection{{Profile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Allow: []string{}}}}}
				case "grant":
					opts.ServerFilter = []string{}
				case "collision":
					opts.ServerFilter = []string{"app", "hidden"}
				}
				opts.Profile = mcpgateway.ProfileSelection{ID: restriction, Profile: &profile}
				view, err := r.NewGatewayView(ctx, a, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer view.Close()
				cs := connectDaemonView(ctx, t, view)
				if cs.InitializeResult().Capabilities.Resources == nil {
					t.Fatal("resource capability absent")
				}
				listed, err := cs.ListResources(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				denied := restriction == "deny" || restriction == "floor" || restriction == "grant" || restriction == "collision"
				if denied && len(listed.Resources) != 0 {
					t.Fatalf("excluded resource listed: %+v", listed)
				}
				if !denied {
					expected := 2
					if restriction == "allow" {
						expected = 1
					}
					if len(listed.Resources) != expected {
						t.Fatalf("all upstream pages not aggregated: %+v", listed)
					}
					found := false
					for _, item := range listed.Resources {
						if item.URI == appResourceURI {
							found = true
							if item.Meta["fixture"] != "resource" {
								t.Fatal("resource metadata lost")
							}
						}
					}
					if !found {
						t.Fatal("ui resource absent")
					}
				}
				before := reads.Load()
				hiddenBefore := hiddenReads.Load()
				result, readErr := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: appResourceURI, Meta: mcpsdk.Meta{"fixture": "request", "Tether.Context": map[string]any{"session_id": "forged"}, "tether.provenance": map[string]any{"session_id": "forged"}}})
				if denied {
					if readErr == nil || reads.Load() != before || hiddenReads.Load() != hiddenBefore {
						t.Fatal("denied resource dispatched")
					}
				} else {
					if readErr != nil {
						t.Fatal(readErr)
					}
					if result.Meta["fixture"] != "read-result" || result.Contents[0].Meta["ui"] == nil || result.Contents[0].MIMEType != "text/html;profile=mcp-app" || !strings.Contains(result.Contents[0].Text, "<html>") {
						t.Fatalf("UI body/metadata changed: %+v", result)
					}
				}
				var audited bool
				for _, event := range pub.got {
					if event.Kind == events.EventTypeToolCallEnd {
						var call events.ToolCallEvent
						if err := json.Unmarshal([]byte(event.PayloadJSON), &call); err != nil {
							t.Fatal(err)
						}
						if call.ToolName == "resources/read" {
							audited = true
							if denied && (call.OK || call.ErrorClass != events.ToolErrorDenied) {
								t.Fatalf("missing denied audit: %+v", call)
							}
						}
						if strings.Contains(event.PayloadJSON, appResourceURI) || strings.Contains(event.PayloadJSON, "<html>") {
							t.Fatal("resource content/URI logged")
						}
					}
				}
				if !audited {
					t.Fatal("resource read not audited")
				}
				templates, err := cs.ListResourceTemplates(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if restriction == "none" {
					if len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].Meta["fixture"] != "template" {
						t.Fatal("template lost")
					}
					if _, err := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "ui://fixture/other.html", Meta: mcpsdk.Meta{"fixture": "request", "Tether.Context": map[string]any{"session_id": "forged"}, "tether.provenance": map[string]any{"session_id": "forged"}}}); err != nil {
						t.Fatal(err)
					}
				}
				before = reads.Load()
				if _, err := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "https://unadvertised.example/secret"}); err == nil || reads.Load() != before {
					t.Fatal("unadvertised read dispatched")
				}
			})
		}
	}
}

func TestResourcePageBindsInventoryAndPolicy(t *testing.T) {
	items := []string{}
	for i := 0; i < mcpsdk.DefaultPageSize+1; i++ {
		items = append(items, fmt.Sprint(i))
	}
	page, cursor, err := resourcePage(items, "", nil)
	if err != nil || len(page) != mcpsdk.DefaultPageSize || cursor == "" {
		t.Fatal("missing page")
	}
	page, next, err := resourcePage(items, cursor, nil)
	if err != nil || len(page) != 1 || next != "" {
		t.Fatal("missing continuation")
	}
	if _, _, err := resourcePage(items[:len(items)-1], cursor, nil); err == nil {
		t.Fatal("stale catalog cursor accepted")
	}
	if _, _, err := resourcePage(items, cursor, &mcpgateway.Policy{Selection: mcpgateway.ProfileSelection{ID: "changed"}}); err == nil {
		t.Fatal("foreign policy cursor accepted")
	}
}

func TestResourceServiceCredentialScrub(t *testing.T) {
	value := &mcpsdk.ReadResourceResult{Meta: mcpsdk.Meta{"canary": "service-canary", "large": json.Number("42")}, Contents: []*mcpsdk.ResourceContents{{URI: appResourceURI, Text: "service-canary", Meta: mcpsdk.Meta{"service-canary": "service-canary"}}}}
	scrubbed, err := scrubResourceResult(value, "service-canary")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "service-canary") || !strings.Contains(string(raw), "42") {
		t.Fatal("credential leaked or metadata changed")
	}
	if value.Contents[0].Text != "service-canary" {
		t.Fatal("upstream result mutated")
	}
}
