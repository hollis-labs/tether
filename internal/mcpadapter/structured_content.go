package mcpadapter

import (
	"bytes"
	"encoding/json"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// objectStructuredContent makes a forwarded tool result acceptable to strict MCP
// clients. The spec requires structuredContent to be a JSON object; some upstreams
// return a bare array or scalar, and a client then rejects the whole call
// ("expected record, received array"). A non-object value is wrapped as
// {"result": value}; the text content is untouched. The upstream result is not
// modified: a changed result is a shallow copy.
func objectStructuredContent(r *mcpsdk.CallToolResult) *mcpsdk.CallToolResult {
	if r == nil || r.StructuredContent == nil || isJSONObject(r.StructuredContent) {
		return r
	}
	cp := *r
	cp.StructuredContent = map[string]any{"result": r.StructuredContent}
	return &cp
}

func isJSONObject(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return true
	case json.RawMessage:
		return startsWithBrace(t)
	case []byte:
		return startsWithBrace(t)
	case []any, string, float64, bool, int, int64:
		return false
	}
	raw, err := json.Marshal(v)
	return err == nil && startsWithBrace(raw)
}

func startsWithBrace(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && b[0] == '{'
}
