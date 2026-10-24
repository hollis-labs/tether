package mcpadapter

import (
	"context"
	"errors"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/telemetry"
)

func (a *Adapter) registerToolMetricsTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{Name: "tether_tool_metrics", Description: "Read counters and cumulative latency histograms for completed tool calls (including rejected calls) from retained durable events. Groups by exact tool, upstream and outcome (and optionally discovery mode); bounded to 1000 groups with an explicit truncated flag. Use compact=true for a small summary. No argument values or identity labels.", InputSchema: gomcp.InputSchema(gomcp.StringProp("tool", "Exact tool name", false), gomcp.StringProp("upstream", "Exact upstream origin", false), gomcp.StringProp("since", "Inclusive RFC3339 lower bound", false), gomcp.StringProp("until", "Exclusive RFC3339 upper bound", false), gomcp.StringEnumProp("mode", "Only calls made in this discovery mode", false, "flat", "search"), gomcp.StringEnumProp("group_by", "Add a grouping dimension: mode splits flat from search", false, "mode"), gomcp.BooleanProp("compact", "Token-cheap rows: averages and p50/p95 bucket bounds instead of full histograms; defaults to the top 25 groups", false), gomcp.IntegerProp("limit", "Max groups returned, highest call count first (1-1000)", false)), Handler: func(ctx context.Context, args map[string]any) (any, error) {
		req := telemetry.MetricsRequest{Tool: str(args, "tool"), Upstream: str(args, "upstream"), Since: str(args, "since"), Until: str(args, "until"), Mode: str(args, "mode"), GroupBy: str(args, "group_by"), Compact: boolArg(args, "compact"), Limit: intArg(args, "limit", 0)}
		if a.readsViaDaemon() {
			out, err := a.client.ToolMetrics(ctx, req)
			if err != nil {
				return nil, classifyClientErr(err, "")
			}
			return out, nil
		}
		if a.svc == nil || a.svc.Store == nil {
			return nil, toolError("internal_error", "tool metric source not configured")
		}
		out, err := telemetry.QueryService{Source: a.svc.Store}.ReadMetrics(ctx, req)
		if err != nil {
			var invalid *telemetry.QueryError
			if errors.As(err, &invalid) {
				return nil, toolError("invalid_request", err.Error())
			}
			return nil, toolError("internal_error", err.Error())
		}
		return out, nil
	}}, Reads("durable tool call metrics"))
}
