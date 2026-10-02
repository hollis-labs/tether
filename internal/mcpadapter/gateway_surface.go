package mcpadapter

import (
	"context"
	"fmt"

	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Keep protocol listing and direct dispatch on the semantic service's boundary.
// Registration alone is not eligibility: unavailable and profile-excluded cached
// definitions must never expose schemas or invoke an SDK-registered handler.
func gatewaySurfaceMiddleware(gateway *mcpgateway.Service) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if call, ok := req.(*mcpsdk.CallToolRequest); ok && !isGatewayTool(call.Params.Name) {
				if _, err := gateway.ResolveTarget(call.Params.Name); err != nil {
					if gateway.Policy == nil || gateway.Policy.Selection.Profile == nil {
						known := false
						for _, e := range gateway.Snapshot().Entries {
							if e.Tool.Name == call.Params.Name {
								known = true
								break
							}
						}
						if !known {
							return next(ctx, method, req)
						}
					}
					return errorResult(err.Error()), nil
				}
			}
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			list, ok := req.(*mcpsdk.ListToolsRequest)
			if !ok {
				return next(ctx, method, req)
			}
			original := list.Params
			if list.Params == nil {
				list.Params = &mcpsdk.ListToolsParams{}
			}
			cursor := list.Params.Cursor
			copyParams := *list.Params
			list.Params = &copyParams
			defer func() { list.Params = original }()
			// Read all SDK pages, so a pinned name cannot disappear merely because it
			// falls after the SDK's alphabetical page boundary. Our cursor binds the
			// resulting live filtered catalog plus the immutable endpoint selection.
			list.Params.Cursor = ""
			var all []*mcpsdk.Tool
			var result *mcpsdk.ListToolsResult
			for {
				raw, err := next(ctx, method, list)
				if err != nil {
					return nil, err
				}
				result, ok = raw.(*mcpsdk.ListToolsResult)
				if !ok {
					return nil, fmt.Errorf("unexpected tools/list result %T", raw)
				}
				all = append(all, result.Tools...)
				if result.NextCursor == "" {
					break
				}
				list.Params.Cursor = result.NextCursor
			}
			tools, cursorOut, err := gateway.ProtocolList(all, isGatewayTool, cursor)
			if err != nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
			}
			result.Tools = tools
			result.NextCursor = cursorOut
			// Filtering changes the page: don't claim cache lifetimes from an SDK page
			// whose members differ. Tool schemas and metadata themselves pass unchanged.
			result.TTLMs = 0
			return result, nil
		}
	}
}
