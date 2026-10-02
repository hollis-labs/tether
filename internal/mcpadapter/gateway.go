package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/hollis-labs/go-mcp/sanitize"
	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (a *Adapter) gatewayService(registry *ToolRegistry, router *ProxyRouter, selection mcpgateway.Selection, tags map[string][]string) *mcpgateway.Service {
	gateway := &mcpgateway.Service{
		Selection: selection,
		Snapshot: func() mcpgateway.Snapshot {
			snapshot := mcpgateway.Snapshot{Entries: []mcpgateway.Entry{}, Origins: []mcpgateway.OriginStatus{}}
			snapshot.Lint, snapshot.Collisions = registry.NameDiagnostics()
			byID := map[string]mcpgateway.OriginStatus{}
			observed := map[string]bool{}
			for _, status := range a.upstreamStatus() {
				observed[status.ID] = true
				byID[status.ID] = mcpgateway.OriginStatus{ID: status.ID, Degraded: status.Degraded, Status: status.Status, ToolCount: status.CatalogedTools, InventoryExamined: status.InventoryExamined, Error: status.Error}
			}
			for _, def := range registry.AllDefinitions() {
				rt, ok := registry.Lookup(def.Name)
				if !ok {
					continue
				}
				origin := rt.ServerID
				if origin == "" {
					origin = "tether"
				}
				snapshot.Entries = append(snapshot.Entries, mcpgateway.Entry{Tool: def, Origin: origin, Tags: tags[rt.ServerID]})
				status, exists := byID[origin]
				if !exists {
					status = mcpgateway.OriginStatus{ID: origin, Status: "connected"}
				}
				if rt.ServerID == "" || !observed[origin] {
					status.ToolCount++
					status.InventoryExamined = true
				}
				if status.Status == "connected" {
					status.AvailableTools++
				}
				byID[origin] = status
			}
			for _, status := range byID {
				snapshot.Origins = append(snapshot.Origins, status)
			}
			sort.Slice(snapshot.Origins, func(i, j int) bool { return snapshot.Origins[i].ID < snapshot.Origins[j].ID })
			return snapshot
		},
		Score: func(query string, entry mcpgateway.Entry) int {
			if query == entry.Tool.Name {
				return len(tokenise(query)) + 1
			}
			words := buildWordSet(entry.Tool.Name, entry.Tool.Description, entry.Tags)
			score := 0
			for word := range tokenise(query) {
				if _, ok := words[word]; ok {
					score++
				}
			}
			return score
		},
		Dispatch: func(ctx context.Context, name string, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
			result, err := router.Handle(ctx, ToolCall{ToolName: name, Args: args, Meta: meta})
			a.recordProxyRefs(ctx, registry, name, args, result, err)
			return result, err
		},
	}
	if a.resolver == nil {
		a.resolver = &routerRefResolver{router: router, gateway: gateway}
	}
	return gateway
}

func discoveryRequest(args map[string]any) (mcpgateway.Request, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return mcpgateway.Request{}, err
	}
	var req mcpgateway.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("invalid discovery arguments: %w", err)
	}
	// An explicit zero limit is not omission; nor is an explicitly empty cursor.
	if _, ok := args["limit"]; ok && req.Limit == 0 {
		return req, fmt.Errorf("limit must be a positive integer")
	}
	if raw, ok := args["detail"]; ok && raw == "" {
		return req, fmt.Errorf("detail must be summary or schema")
	}
	return req, nil
}

func arrayProperty() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}
func discoverySchema(search bool) map[string]any {
	properties := map[string]any{"servers": arrayProperty(), "cursor": map[string]any{"type": "string"}}
	maxLimit, defaultLimit := 100, 50
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if search {
		maxLimit, defaultLimit = 50, 10
		properties["query"] = map[string]any{"type": "string", "minLength": 1}
		properties["tags"] = arrayProperty()
		properties["detail"] = map[string]any{"type": "string", "enum": []string{"summary", "schema"}, "default": "summary"}
		schema["required"] = []string{"query"}
	} else {
		properties["names"] = arrayProperty()
	}
	properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "default": defaultLimit}
	return schema
}
func (a *Adapter) registerSearchTool(s *gomcp.Server, gateway *mcpgateway.Service) {
	a.addTool(s, gomcp.Tool{Name: "tether_tool_search", Description: "Search eligible tools by a nonempty query. Use detail=schema for the real input/output schemas; otherwise hydrate exact names with tether_tool_list. Filters narrow this endpoint's inventory; results report incomplete upstream discovery. Execute with tether_tool_call.", InputSchema: discoverySchema(true), Handler: func(_ context.Context, args map[string]any) (any, error) {
		req, err := discoveryRequest(args)
		if err != nil {
			return nil, toolError("invalid_request", err.Error())
		}
		out, err := gateway.Search(req)
		if err != nil {
			return nil, toolError("invalid_request", err.Error())
		}
		return out, nil
	}}, Reads("eligible tool metadata"))
}
func (a *Adapter) registerListTool(s *gomcp.Server, gateway *mcpgateway.Service) {
	a.addTool(s, gomcp.Tool{Name: "tether_tool_list", Description: "Enumerate eligible tools deterministically with real schemas, or hydrate exact names using names (mutually exclusive with servers/limit/cursor). Unknown, excluded and unavailable names return per-name errors. No filter bypass.", InputSchema: discoverySchema(false), Handler: func(_ context.Context, args map[string]any) (any, error) {
		req, err := discoveryRequest(args)
		if err != nil {
			return nil, toolError("invalid_request", err.Error())
		}
		out, err := gateway.List(req)
		if err != nil {
			return nil, toolError("invalid_request", err.Error())
		}
		return out, nil
	}}, Reads("eligible tool schemas"))
}
func (a *Adapter) registerCallTool(s *gomcp.Server, gateway *mcpgateway.Service) {
	s.SDKServer().AddTool(&mcpsdk.Tool{Name: "tether_tool_call", Description: "Dispatch one eligible exact tool name using its real arguments schema. Unknown, excluded or unavailable targets fail. In search mode client permissions and hooks see tether_tool_call, not the downstream tool identity; this dispatcher may mutate state and is not read-only.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "arguments"}, "properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}, "arguments": map[string]any{"type": "object"}}}, Annotations: Destroys("dispatches targets that may irreversibly remove information or stop sessions").OpenWorld().annotations().sdk()}, a.rawProxyHandler("tether_tool_call", func(ctx context.Context, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return errorResult("name is required"), nil
		}
		arguments, ok := args["arguments"].(map[string]any)
		if !ok {
			return errorResult("arguments must be a JSON object"), nil
		}
		arguments, report := sanitize.Sanitize(arguments)
		if report.Changed() {
			a.logger().Warn("mcp-sanitize: cleaned dispatched tool call", "tool", name, "fields_cleaned", report.FieldsCleaned, "dropped_count", len(report.DroppedFragments))
		}
		result, err := gateway.Call(ctx, name, arguments, meta)
		if err != nil {
			var targetErr *mcpgateway.TargetError
			if errors.As(err, &targetErr) {
				return errorResult(err.Error()), nil
			}
			return nil, err
		}
		return result, nil
	}))
}
func (a *Adapter) registerGatewayStatus(s *gomcp.Server, gateway *mcpgateway.Service) {
	a.addTool(s, gomcp.Tool{Name: "tether_gateway_status", Description: "Explain effective profile/mode and source, cataloged/eligible/available tool counts, exclusions by reason, upstream errors, collisions and metadata lint. An optional exact name reports its visibility and authored annotations. Cached or unexamined inventory never proves availability.", InputSchema: gomcp.InputSchema(gomcp.StringProp("name", "Optional exact tool name to explain", false)), Handler: func(_ context.Context, args map[string]any) (any, error) {
		name, _ := args["name"].(string)
		out := gateway.Status(name)
		if isGatewayTool(name) {
			visible := name == "tether_gateway_status" || gateway.Selection.Mode == mcpgateway.Search
			out.Visible = &visible
			out.Reason = "gateway discovery infrastructure"
			if !visible {
				out.Reason = "flat mode lists real tools directly and omits discovery/dispatch wrappers"
			}
		}
		return out, nil
	}}, Reads("gateway mode and upstream status"))
}
func isGatewayTool(name string) bool {
	return name == "tether_tool_search" || name == "tether_tool_list" || name == "tether_tool_call" || name == "tether_gateway_status"
}
