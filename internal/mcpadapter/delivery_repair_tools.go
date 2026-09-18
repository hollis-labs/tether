// Package mcpadapter — delivery_repair_tools.go wires the native
// mux_message_trace / mux_message_redrive / mux_message_retention_candidates /
// mux_message_purge MCP tools (T09, messaging vNext), giving MCP callers
// parity with GET /messages/{id}/trace, POST /messages/{id}/redrive, GET
// /messages/retention/candidates and POST /messages/{id}/purge
// (internal/api/trace.go, repair.go, retention.go).
package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/go-mcp/server"
)

// ScopeDeliveryWrite gates the redrive tool. Deliberately a SEPARATE
// scope from message.write (matching this package's established
// one-scope-per-capability-group convention, e.g. registry.write vs
// groups.write) -- an operator can grant repair capability without also
// granting ordinary send/consume/cancel capability, and vice versa.
// Trace is read-only and requires no scope, matching every other
// read-only tool in this package.
const ScopeDeliveryWrite = "delivery.write"

func (a *Adapter) registerDeliveryRepairTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_message_trace",
		Description: "Show the structured delivery trace for a message: who sent to whom, why a " +
			"binding resolved, which host accepted, which turn was submitted, and why " +
			"retry/expiry occurred. Read-only; no scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID."),
		}, "message_id"),
		Handler: a.handleMessageTraceTool,
	}, Reads("GET /messages/{id}/trace"))

	a.addTool(s, gomcp.Tool{
		Name: "mux_message_redrive",
		Description: "Authorized retry of a dead-lettered delivery. Idempotent: calling this again " +
			"on an already-retryable delivery reports redriven=false, not an error. " +
			"message_id may be a literal delivery id to address one specific group-fanout " +
			"recipient's delivery (use mux_message_trace or an operator's own inspection " +
			"to find it). Requires the delivery.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id":           strProp("Message ID, or a literal delivery id for a group-fanout recipient."),
			"authorized_by":        strProp("URN recorded as provenance for this repair (self-asserted, ADR 0045)."),
			"new_deadline_seconds": numProp("New delivery deadline in seconds from now; 0 or omitted means no deadline."),
		}, "message_id", "authorized_by"),
		Handler: a.handleMessageRedriveTool,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "mux_message_retention_candidates",
		Description: "Preview messages eligible for a body purge -- read-only, mutates nothing. " +
			"Use this before mux_message_purge to check a message's eligibility " +
			"(a pending, leased, retry_scheduled or dead-lettered delivery is never " +
			"eligible; see mux_message_purge's description for why dead-lettered is " +
			"excluded). No scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"older_than_hours": numProp("Lookback window in hours; 0 or omitted uses the daemon's default."),
		}),
		Handler: a.handleMessageRetentionCandidatesTool,
	}, Reads("GET /messages/retention/candidates: reports, deletes nothing"))

	a.addTool(s, gomcp.Tool{
		Name: "mux_message_purge",
		Description: "Clear one message's body/metadata, leaving its structural/trace fields " +
			"(id, kind, from, to, thread, timestamps) intact. Irreversible. Refuses " +
			"with an error when the message has a pending delivery obligation -- " +
			"including dead-lettered, which remains repairable via mux_message_redrive " +
			"and would resend an empty message if purged first. Idempotent: purging " +
			"an already-purged message reports purged=false, not an error. Requires " +
			"the delivery.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id":    strProp("Message ID."),
			"authorized_by": strProp("URN recorded as provenance for this purge (self-asserted, ADR 0045)."),
		}, "message_id", "authorized_by"),
		Handler: a.handleMessagePurgeTool,
	}, Destroys("permanently deletes messages; irreversible"))
}

func (a *Adapter) handleMessageTraceTool(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "message_id")
	if id == "" {
		return nil, toolError("invalid_request", "message_id is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_trace requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.MessageTrace(ctx, id)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "trace": out}), nil
}

func (a *Adapter) handleMessageRedriveTool(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeDeliveryWrite); err != nil {
		return nil, err
	}
	id := str(args, "message_id")
	authorizedBy := str(args, "authorized_by")
	if id == "" || authorizedBy == "" {
		return nil, toolError("invalid_request", "message_id and authorized_by are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_redrive requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.MessageRedrive(ctx, id, authorizedBy, intArg(args, "new_deadline_seconds", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "result": out}), nil
}

func (a *Adapter) handleMessageRetentionCandidatesTool(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_retention_candidates requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.MessageRetentionCandidates(ctx, intArg(args, "older_than_hours", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "candidates": out}), nil
}

func (a *Adapter) handleMessagePurgeTool(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeDeliveryWrite); err != nil {
		return nil, err
	}
	id := str(args, "message_id")
	authorizedBy := str(args, "authorized_by")
	if id == "" || authorizedBy == "" {
		return nil, toolError("invalid_request", "message_id and authorized_by are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_purge requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.MessagePurge(ctx, id, authorizedBy)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "result": out}), nil
}
