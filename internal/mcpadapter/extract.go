package mcpadapter

import (
	"context"
	"encoding/json"

	"github.com/hollis-labs/tether/internal/app/sessionrefs"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResolvedRef retains the application resolver contract at the composition seam.
type ResolvedRef = sessionrefs.ResolvedRef
type RefResolver = sessionrefs.RefResolver
type refAttacher = sessionrefs.Attacher
type extractedRef = sessionrefs.Ref

const (
	refKindTorqueTask        = sessionrefs.KindTorqueTask
	refKindTesseractItem     = sessionrefs.KindTesseractItem
	refKindTesseractRevision = sessionrefs.KindTesseractRevision
	refKindTesseractKey      = sessionrefs.KindTesseractKey
	refKindMessagingURN      = sessionrefs.KindMessagingURN
	relationCreated          = sessionrefs.RelationCreated
	relationUpdated          = sessionrefs.RelationUpdated
	relationRead             = sessionrefs.RelationRead
	relationReferenced       = sessionrefs.RelationReferenced
	maxScanDepth             = sessionrefs.MaxScanDepth
	maxScanValues            = sessionrefs.MaxScanValues
)

type scanResult struct {
	refs    []extractedRef
	refused bool
}

func extractRefs(toolName string, args map[string]any) scanResult {
	result := sessionrefs.ExtractArguments(toolName, args)
	return scanResult{refs: result.Refs, refused: result.Refused}
}
func relationForTool(toolName string) string { return sessionrefs.RelationForTool(toolName) }

// parseResultMap extracts the top-level structured JSON map from a CallToolResult.
func parseResultMap(res *mcpsdk.CallToolResult) map[string]any {
	if res == nil {
		return nil
	}
	if m, ok := res.StructuredContent.(map[string]any); ok && len(m) > 0 {
		return m
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok && tc != nil {
			var m map[string]any
			if err := json.Unmarshal([]byte(tc.Text), &m); err == nil && len(m) > 0 {
				return m
			}
		}
	}
	return nil
}

func (a *Adapter) recordProxyRefs(ctx context.Context, registry *ToolRegistry, name string, args map[string]any, res *mcpsdk.CallToolResult, callErr error) {
	if registry == nil {
		return
	}
	if rt, ok := registry.Lookup(name); ok {
		a.recordRefs(ctx, rt.UpstreamName, args, res, callErr)
	}
}

// recordRefs translates SDK observations and invokes the application policy.
func (a *Adapter) recordRefs(ctx context.Context, toolName string, args map[string]any, res *mcpsdk.CallToolResult, callErr error) {
	operation := sessionrefs.New(a.ExtractRefs, a.SessionID, a.resolver, a.refs, a.logger())
	if !operation.Enabled() {
		return
	}
	call := sessionrefs.Observation{ToolName: toolName, Arguments: args, Failed: callErr != nil || (res != nil && res.IsError)}
	if !call.Failed {
		call.Result = parseResultMap(res)
	}
	operation.Record(ctx, call)
}
