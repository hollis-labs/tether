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
// not copied argument/result payloads. Scrubbed error text may echo argument fragments. GatewayMs is pre/post-dispatch overhead;
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

	// ClientLabel is the caller's self-declared name (CW-20261024-0048). It is a
	// claim, never identity: operator-token clients share one verified principal,
	// so this is the only way to tell them apart in telemetry. Bounded and
	// charset-restricted at the edge; never used for authorization.
	ClientLabel string `json:"client_label,omitempty"`
	// MCPSession is a short hash of the transport session handle, grouping one
	// client connection's calls without storing the handle itself.
	MCPSession string `json:"mcp_session,omitempty"`

	// Search describes a tether_tool_search call; nothing here stores argument
	// values other than the normalized query terms (a query is not a secret).
	// Kept comparable (no slices or pointers): callers compare details with ==.
	// A search event is identified by SearchID; SearchTerms is space-joined.
	SearchID        string `json:"search_id,omitempty"`
	SearchTerms     string `json:"search_terms,omitempty"`
	SearchResults   int    `json:"search_results,omitempty"`
	SearchZero      bool   `json:"search_zero,omitempty"`
	SearchDetail    string `json:"search_detail,omitempty"`
	SearchTruncated bool   `json:"search_truncated,omitempty"`

	// AfterSearchID/Rank link a later call to the search that preceded it from the
	// same connection: Rank is the 1-based position of the called tool in that
	// search's results (0 = it was not among them).
	AfterSearchID   string `json:"after_search_id,omitempty"`
	AfterSearchRank int    `json:"after_search_rank,omitempty"`
	AfterSearchMs   int64  `json:"after_search_ms,omitempty"`

	// ResolvedTool is the downstream tool a tether_tool_call dispatch targeted.
	ResolvedTool string `json:"resolved_tool,omitempty"`
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
