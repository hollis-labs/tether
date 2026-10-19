package events

import (
	"encoding/json"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
)

// CanonicalToolCall constructs the metadata-only catalog v1 observation.
// Deliberately whitelist fields: legacy error text can echo argument/result
// content even after credential redaction. Attribution belongs in the envelope.
func CanonicalToolCall(call ToolCallEvent, finished bool) Event {
	payload := map[string]any{
		"tool": call.ToolName, "server": call.Server,
		"args_bytes": call.ArgsBytes, "args_schema_fp": call.ArgsSchemaFP,
	}
	kind := catalog.KindToolCallStarted
	if finished {
		kind = catalog.KindToolCallFinished
		payload["ok"] = call.OK
		payload["error_class"] = call.ErrorClass
		payload["duration_ms"] = call.DurationMs
		payload["gateway_ms"] = call.GatewayMs
		payload["forward_ms"] = call.ForwardMs
		payload["result_bytes"] = call.ResultBytes
	}
	// The payload contains only JSON scalar types.
	raw, _ := json.Marshal(payload)
	ev := Event{At: call.Timestamp, Scope: ScopeDaemon, Kind: kind, PayloadJSON: string(raw)}
	if call.Attribution.SessionResolved && call.Attribution.SessionID != "" && call.Attribution.SessionID == call.SessionID {
		ev.Scope, ev.SessionID = ScopeSession, call.SessionID
	}
	// Trace/span metadata identifies the observed call, not a verified actor.
	// Legacy proxy ingress already validates these identifiers before conversion.
	ev.Envelope.Corr = Corr{TraceID: call.TraceID, SpanID: call.SpanID}
	ev.Envelope.Class = Class{Category: "tool", PayloadSchema: "urn:hollis-labs:events:" + kind + "@1"}
	return ev
}
