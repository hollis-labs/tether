package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDocsThroughSharedGatewayResourcesAndTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"flat", "search"} {
		for _, deny := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/denied", false: "/visible"}[deny], func(t *testing.T) {
				ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "docs-reader", Kind: "service"})
				a := newTestAdapter(t)
				a.principal = &identity.Principal{ID: "docs-reader", Kind: "service"}
				a.svc.Catalog = &config.Catalog{}
				r, err := NewSharedUpstreams(nil, daemonTestRoots(t), true)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				profile := &mcpgateway.Profile{}
				if deny {
					profile.Tools.Deny = []string{"tether_docs_*"}
				}
				view, err := r.NewGatewayView(ctx, a, ProxyOptions{Profile: mcpgateway.ProfileSelection{Profile: profile}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: mode, Source: "test"}}}})
				if err != nil {
					t.Fatal(err)
				}
				defer view.Close()
				client := connectDaemonView(ctx, t, view)
				resources, err := client.ListResources(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, resource := range resources.Resources {
					if resource.URI == "tether://docs/mcp/connect" {
						found = true
					}
				}
				if found == deny {
					t.Fatalf("resource visibility: found=%v deny=%v", found, deny)
				}
				read, readErr := client.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "tether://docs/mcp/connect"})
				if deny {
					for _, resource := range resources.Resources {
						if strings.HasPrefix(resource.URI, "tether://docs/") {
							t.Fatal("denied documentation metadata remains visible")
						}
					}
					if readErr == nil {
						t.Fatal("resource bypassed denied docs tool")
					}
					if _, err := client.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "tether://docs/files/docs/mcp.md"}); err == nil {
						t.Fatal("reference resource bypassed denied file tool")
					}
					return
				}
				if readErr != nil {
					t.Fatal(readErr)
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(read.Contents[0].Text), &body); err != nil {
					t.Fatal(err)
				}
				call := func(name string, args map[string]any) *mcpsdk.CallToolResult {
					if mode == "search" {
						args = map[string]any{"name": name, "arguments": args}
						name = "tether_tool_call"
					}
					result, err := client.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
					if err != nil || result.IsError {
						t.Fatalf("%s: %v %v", name, result, err)
					}
					return result
				}
				listed := call("tether_docs_list", map[string]any{})
				if strings.Contains(textOf(listed), `"body"`) || strings.Contains(textOf(listed), `"files"`) {
					t.Fatal("list leaked lower-tier content")
				}
				got := parseToolJSON(t, call("tether_docs_get", map[string]any{"id": "connect"}))
				if got["body"] != body["body"] {
					t.Fatal("resource and tool body differ")
				}
				files := got["files"].([]any)
				if len(files) == 0 {
					t.Fatal("guide references missing")
				}
				file := files[0].(map[string]any)
				fetched := parseToolJSON(t, call("tether_docs_get_file", map[string]any{"id": "connect", "path": file["path"]}))
				ref, err := client.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: file["uri"].(string)})
				if err != nil || ref.Contents[0].Text != fetched["body"] {
					t.Fatalf("reference resource differs: %v", err)
				}
			})
		}
	}
}
