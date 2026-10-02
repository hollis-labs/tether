package proxyevents

import (
	"github.com/hollis-labs/tether/internal/events"
	"time"
)

// IngestCall translates one secret-free observation for the credentialed daemon
// sink. Attribution is deliberately omitted: the daemon resolves it again.
func IngestCall(call events.ToolCallEvent, phase string, publish bool) ProxyEventIngestRequest {
	details := call.ToolCallDetails
	if len(call.Error) > MaxProxyEventErrorBytes {
		details.ErrorTruncated = true
	}
	return ProxyEventIngestRequest{ToolCallDetails: details, ClaimedSessionID: call.ClaimedSessionID, SessionID: call.SessionID, Server: call.Server, ToolName: call.ToolName, ArgsSchemaFP: call.ArgsSchemaFP, DurationMs: call.DurationMs, OK: call.OK, Error: TruncateProxyEventError(call.Error), Timestamp: call.Timestamp.UTC().Format(time.RFC3339Nano), Phase: phase, Publish: publish}
}
