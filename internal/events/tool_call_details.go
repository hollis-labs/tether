package events

import "unicode/utf8"

type ToolErrorClass string

const (
	ToolErrorDenied       ToolErrorClass = "denied"
	ToolErrorUpstreamDown ToolErrorClass = "upstream_down"
	ToolErrorTimeout      ToolErrorClass = "timeout"
	ToolErrorValidation   ToolErrorClass = "validation"
	ToolErrorUpstream     ToolErrorClass = "upstream_error"
	MaxToolCallErrorBytes                = 4 << 10
)

// ToolCallDetails is additive metadata; byte sizes describe JSON encoding,
// never retained argument/result values. GatewayMs is pre/post-dispatch overhead;
// ForwardMs is dispatch wall time. ErrorTruncated refers to retained error text.
type ToolCallDetails struct {
	Profile        string         `json:"profile,omitempty"`
	DiscoveryMode  string         `json:"discovery_mode,omitempty"`
	ArgsBytes      int64          `json:"args_bytes"`
	ResultBytes    int64          `json:"result_bytes"`
	ErrorTruncated bool           `json:"error_truncated"`
	ErrorClass     ToolErrorClass `json:"error_class,omitempty"`
	TraceID        string         `json:"trace_id,omitempty"`
	SpanID         string         `json:"span_id,omitempty"`
	GatewayMs      int64          `json:"gateway_ms"`
	ForwardMs      int64          `json:"forward_ms"`
}

func TruncateToolCallError(s string) string {
	if len(s) <= MaxToolCallErrorBytes {
		return s
	}
	const marker = "…[truncated]"
	n := MaxToolCallErrorBytes - len(marker)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + marker
}
