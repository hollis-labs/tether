package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/libs/plugin-mcp/go-mcp/budget"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/redact"
	"github.com/hollis-labs/tether/internal/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func recordTargetError(ctx context.Context, err error) {
	var target *mcpgateway.TargetError
	if errors.As(err, &target) {
		class := events.ToolErrorDenied
		if target.Unavailable {
			class = events.ToolErrorUpstreamDown
		}
		telemetry.SetErrorClass(ctx, class)
	}
}

// Native calls cross an in-memory SDK session, which carries structured codes
// rather than the outer call's context values. Classify that contract only.
func nativeResultClass(result *mcpsdk.CallToolResult) events.ToolErrorClass {
	switch value := result.StructuredContent.(type) {
	case map[string]any:
		code, _ := value["code"].(string)
		return telemetry.NativeErrorClass(code)
	case *budget.ToolError:
		return telemetry.NativeErrorClass(value.Code)
	default:
		return events.ToolErrorUpstream
	}
}

// scrubStructuredError copies the JSON error object. Successful results never
// reach this path. Numbers retain their JSON representation and inputs are not
// mutated; only string values are scrubbed with the configured credential set.
func scrubStructuredError(value any, secrets *redact.Set) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return secrets.Redact(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = scrubStructuredError(item, secrets)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = scrubStructuredError(item, secrets)
		}
		return out
	case bool, json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
		return value
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return value
		}
		return scrubStructuredError(decoded, secrets)
	}
}
