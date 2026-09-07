package mcpadapter

// selfdiscovery.go — T08 (messaging vNext, CW-20260906-0039): MCP tools
// for GET /whoami and POST /bootstrap/session. Both compose registry
// state AND live local-session state (RuntimeHealth/ResolveActorSession),
// the same cross-cutting shape messages.go's notify/wake tools have --
// so, like those (T05), these route through the daemon HTTP client
// (a.client) rather than registry_tools.go's direct-to-a.svc.Registry
// convention, which is reserved for tools that touch registry state
// alone.

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/client"
)

func (a *Adapter) registerSelfDiscoveryTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_whoami",
		mcp.WithDescription(
			"Self-discovery for a claimed URN (same-host, self-asserted trust model, ADR 0045 -- "+
				"this reports what Tether currently knows about the identity, it does not verify "+
				"the claim). Returns the registry profile, current RuntimeBinding, group "+
				"memberships, and (when it currently resolves to a session THIS daemon's runtime "+
				"is actually running) the session id, host, and negotiated provider capabilities. "+
				"Every section is independent and best-effort -- a bare, otherwise-unknown URN "+
				"still returns 'ok' with an otherwise-empty result. Read-only; no scope required.",
		),
		mcp.WithString("urn", mcp.Required(), mcp.Description("The URN to self-discover, e.g. msg://agent/agent-mux/agt_xxxxxxxxxx.")),
	), a.handleWhoAmI)

	a.addTool(s, mcp.NewTool("tether_bootstrap_session",
		mcp.WithDescription(
			"Provider-neutral local bootstrap/registration helper for the launch/host boundary "+
				"(e.g. agent-setup). Idempotently resolves a preassigned SESSION into (optionally) "+
				"a registered, bound durable actor identity in one call.\n"+
				"logical_agent_id absent: standalone, session-only boot -- no registry write, no binding.\n"+
				"logical_agent_id present, publication omitted/'private-local': private offline boot "+
				"-- identity by naming only, still no registry write or binding (only Tether's own "+
				"internal launch path mints a private-local binding).\n"+
				"logical_agent_id present, publication='published-local': registers (idempotently, "+
				"by logical_agent_id) and leases a binding (capabilities locked to pull-only). A "+
				"repeated call with the same session_id/host_id/attempt_id is a true no-op; a "+
				"genuinely new attempt_id is a legitimate reconnect. Requires the registry.write scope.",
		),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The preassigned SESSION identity.")),
		mcp.WithString("intent", mcp.Description("Informational passthrough: preassigned, resume, fresh, fork, compact.")),
		mcp.WithString("logical_agent_id", mcp.Description("Presence implies a durable actor, not a one-off session.")),
		mcp.WithString("display_name", mcp.Description("Registry display name; defaults to logical_agent_id.")),
		mcp.WithString("provider_id", mcp.Description("Optional; accepted but not consumed by this call.")),
		mcp.WithString("publication", mcp.Description("private-local (default) or published-local.")),
		mcp.WithString("host_id", mcp.Description("Defaults to \"external-bootstrap\" when a binding is leased.")),
		mcp.WithString("attempt_id", mcp.Description("Defaults to session_id; a new value signals a reconnect.")),
		mcp.WithString("substrate", mcp.Description("Defaults to \"agent-setup\".")),
	), a.handleBootstrapSession)
}

func (a *Adapter) handleWhoAmI(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_whoami requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.WhoAmI(ctx, urn)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, urn), nil
	}
	return toolJSON(map[string]any{"ok": true, "whoami": out}), nil
}

func (a *Adapter) handleBootstrapSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeRegistryWrite); denied != nil {
		return denied, nil
	}
	sessionID := str(req, "session_id")
	if sessionID == "" {
		return toolError("invalid_request", "session_id is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_bootstrap_session requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.BootstrapSession(ctx, client.BootstrapSessionRequest{
		SessionID:      sessionID,
		Intent:         str(req, "intent"),
		LogicalAgentID: str(req, "logical_agent_id"),
		DisplayName:    str(req, "display_name"),
		ProviderID:     str(req, "provider_id"),
		Publication:    str(req, "publication"),
		HostID:         str(req, "host_id"),
		AttemptID:      str(req, "attempt_id"),
		Substrate:      str(req, "substrate"),
	})
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, sessionID), nil
	}
	return toolJSON(map[string]any{"ok": true, "bootstrap": out}), nil
}
