package mcpadapter

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// resultCapBase is the daemon default cap: the environment override when set
// (0 or negative turns the cap off), else mcpgateway.DefaultResultCapBytes.
func resultCapBase(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(mcpgateway.ResultCapEnv))
	if raw == "" {
		return mcpgateway.DefaultResultCapBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return mcpgateway.DefaultResultCapBytes
	}
	return n
}

// ResultCapMiddleware cuts oversize read results at an item boundary and stores
// the rest behind a cursor (CW-20261024-0052). It sits inside the logging
// middleware so the event records the size it received and the size it sent.
type ResultCapMiddleware struct {
	pager    *mcpgateway.ResultPager
	cap      *mcpgateway.ResultCap
	base     int
	registry *ToolRegistry
}

func (a *Adapter) resultPager() *mcpgateway.ResultPager {
	a.pagerOnce.Do(func() { a.pagerStore = mcpgateway.NewResultPager() })
	return a.pagerStore
}

// newResultCapMiddleware builds the cap for one gateway view. profile may be nil.
func (a *Adapter) newResultCapMiddleware(registry *ToolRegistry, profile *mcpgateway.Profile) *ResultCapMiddleware {
	m := &ResultCapMiddleware{pager: a.resultPager(), base: resultCapBase(os.Getenv), registry: registry}
	if profile != nil {
		m.cap = profile.ResultCap
	}
	return m
}

func (m *ResultCapMiddleware) readOnly(name string) bool {
	rt, ok := m.registry.Lookup(name)
	if !ok || rt.Definition == nil || rt.Definition.Annotations == nil {
		return false
	}
	return rt.Definition.Annotations.ReadOnlyHint
}

// Handle implements ToolCallMiddleware.
func (m *ResultCapMiddleware) Handle(ctx context.Context, call ToolCall, next ToolCallHandler) (*mcpsdk.CallToolResult, error) {
	result, err := next(ctx, call)
	if err != nil || result == nil || call.ToolName == mcpgateway.ResultNextTool {
		return result, err
	}
	limit := m.cap.For(call.ToolName, m.base)
	if limit <= 0 {
		return result, nil
	}
	out, rep := m.pager.Cap(call.ToolName, m.readOnly(call.ToolName), result, limit)
	if rep.Truncated || rep.Skipped != "" {
		telemetry.SetResultCap(ctx, rep.OriginalBytes, rep.Truncated, rep.Skipped)
	}
	return out, nil
}

// registerResultNextTool registers tether_result_next, which serves the next slice
// of a result the gateway cap cut. It is registered raw so the page is delivered
// byte for byte, not wrapped a second time into text and structured copies.
func (a *Adapter) registerResultNextTool(s *gomcp.Server) {
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"cursor"}, "properties": map[string]any{
		"cursor":    map[string]any{"type": "string", "description": "next_cursor from a result's gateway_truncated marker"},
		"max_bytes": map[string]any{"type": "integer", "minimum": 1024, "description": "Optional page size in bytes (default " + strconv.Itoa(mcpgateway.DefaultResultCapBytes) + ")"},
	}}
	s.SDKServer().AddTool(&mcpsdk.Tool{
		Name:        mcpgateway.ResultNextTool,
		Description: "Fetch the next slice of a tool result the gateway cut for size. Pass the next_cursor from the gateway_truncated marker. Pages come from a stored copy of the original result, so they never skip or repeat items even if the data changes; the copy is kept for ten minutes, after which call the original tool again.",
		InputSchema: schema,
		Annotations: Reads("stored copy of a capped result").annotations().sdk(),
	}, a.rawProxyHandler(mcpgateway.ResultNextTool, func(_ context.Context, args, _ map[string]any) (*mcpsdk.CallToolResult, error) {
		cursor, _ := args["cursor"].(string)
		if cursor == "" {
			return errorResult("cursor is required"), nil
		}
		limit := 0
		switch v := args["max_bytes"].(type) {
		case float64:
			limit = int(v)
		case json.Number:
			n, _ := v.Int64()
			limit = int(n)
		}
		if limit != 0 && limit < 1024 {
			return errorResult("max_bytes must be at least 1024"), nil
		}
		page, _, err := a.resultPager().Next(cursor, limit)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		return page, nil
	}))
}
