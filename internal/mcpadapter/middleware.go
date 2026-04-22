// Package mcpadapter — middleware.go holds helpers for composing ToolCallMiddleware
// chains around the ProxyRouter's core dispatch logic.
//
// The ToolCallMiddleware interface and ToolCallHandler type live in proxy.go
// (co-located with ProxyRouter). This file provides buildMiddlewareChain,
// argsSchemaFP and related utilities that are shared across implementations.
//
// See ADR 0021 §Decision 1 for the design rationale.
package mcpadapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
)

// buildMiddlewareChain composes a slice of middleware around a terminal handler,
// returning a single ToolCallHandler. The first middleware in the slice is the
// outermost wrapper (runs first on entry, last on return). An empty slice
// returns handler unchanged.
func buildMiddlewareChain(handler ToolCallHandler, mws []ToolCallMiddleware) ToolCallHandler {
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		inner := handler
		// Capture loop variables explicitly.
		mwCopy := mw
		innerCopy := inner
		handler = func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mwCopy.Handle(ctx, req, innerCopy)
		}
	}
	return handler
}

// argsSchemaFP computes an 8-character hex SHA-256 of the sorted arg key names
// in a JSON object. Arg values are never read or logged. See ADR 0021 §Decision 3.
func argsSchemaFP(args json.RawMessage) string {
	if len(args) == 0 {
		return "00000000"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return "00000000"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for i, k := range keys {
		if i > 0 {
			h.Write([]byte(","))
		}
		h.Write([]byte(k))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}
