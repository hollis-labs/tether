package events

import "time"

// EventTypeToolCallStart is emitted by LoggingMiddleware immediately before
// a proxied tool call is forwarded to an upstream MCP server.
// PayloadJSON carries a partial ToolCallEvent with Timestamp, SessionID,
// ToolName, and Server (DurationMs and OK are not yet known).
const EventTypeToolCallStart = "tool_call_start"

// EventTypeToolCallEnd is emitted by LoggingMiddleware after the upstream
// call completes (or errors). PayloadJSON carries a full ToolCallEvent.
const EventTypeToolCallEnd = "tool_call_end"

// ToolCallEvent is the structured payload emitted for every proxied tool call.
// It is serialised to JSON and stored in Event.PayloadJSON when published to
// the Bus. Consumers that want the typed struct should unmarshal PayloadJSON.
//
// Privacy note: ArgsSchemaFP is derived from arg key names only — never from
// arg values. This ensures tokens, passwords and other secrets never appear in
// the event log. See ADR 0021.
type ToolCallEvent struct {
	// SessionID is the mux session that originated the call, if known.
	// Empty string when the call came from outside a session context.
	SessionID string `json:"session_id,omitempty"`

	// ToolName is the exact tool name as received in tools/call.
	ToolName string `json:"tool_name"`

	// Server is the upstream MCPServerEntry.ID the call was routed to.
	// Empty for native mux tools (those do not pass through LoggingMiddleware).
	Server string `json:"server"`

	// ArgsSchemaFP is an 8-character hex SHA-256 fingerprint of the sorted
	// arg key names. Never contains arg values.
	ArgsSchemaFP string `json:"args_schema_fp"`

	// DurationMs is the round-trip time in milliseconds from just before the
	// upstream call to just after it returned. Zero in tool_call_start events.
	DurationMs int64 `json:"duration_ms"`

	// OK is false when the upstream returned IsError:true or returned a
	// non-nil transport error. False in tool_call_start events.
	OK bool `json:"ok"`

	// Error is the error message when OK is false. Empty when OK is true.
	Error string `json:"error,omitempty"`

	// Timestamp is when the event was created (start or end, depending on
	// event type).
	Timestamp time.Time `json:"timestamp"`
}
