package mcpadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool-level failures are successful JSON-RPC replies, so the normal Go-error
// redaction path never sees them. Scrub the daemon-only service credential
// from error content/structured details before a failure reaches its caller.
func scrubServiceToolError(result *mcpsdk.CallToolResult, secret string) (*mcpsdk.CallToolResult, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("upstream tool failure could not be scrubbed")
	}
	if !bytes.Contains(raw, []byte(secret)) {
		return result, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("upstream tool failure could not be scrubbed")
	}
	raw, err = json.Marshal(scrubServiceValue(value, secret))
	if err != nil {
		return nil, fmt.Errorf("upstream tool failure could not be scrubbed")
	}
	var scrubbed mcpsdk.CallToolResult
	if err := json.Unmarshal(raw, &scrubbed); err != nil {
		return nil, fmt.Errorf("upstream tool failure could not be scrubbed")
	}
	return &scrubbed, nil
}

func scrubServiceValue(value any, secret string) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, secret, "[redacted]")
	case json.Number:
		if string(v) == secret {
			return "[redacted]"
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = scrubServiceValue(item, secret)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[strings.ReplaceAll(k, secret, "[redacted]")] = scrubServiceValue(item, secret)
		}
		return out
	default:
		return value
	}
}
