// Package mcpadapter — whoami_tools.go wires the native tether_whoami
// MCP tool (T08, messaging vNext), giving MCP callers parity with GET
// /whoami (internal/api/whoami.go) -- CLI parity landed the same task as
// `tether whoami`.
package mcpadapter

import (
	"context"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/registry"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
)

// registerWhoamiTools wires tether_whoami onto s. Read-only; no scope
// (same-host UDS trust, ADR 0045 -- as is a self-asserted claim).
func (a *Adapter) registerWhoamiTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_whoami",
		Description: "Self-discovery: the registered Profile (if any), attached external-id " +
			"mappings, group memberships, and the current RuntimeBinding (host/session " +
			"currently owning delivery), if any, for the claimed URN. An unregistered " +
			"or never-bound identity is not an error -- every field is independently " +
			"best-effort.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("as", "msg:// URN to look up (self-asserted, no verification). Omit to look up yourself: your verified principal and inbox address are derived from your session credential.", false),
		),
		Handler: a.handleWhoami,
	}, Reads("resolves the caller identity; asserts nothing"))
}

func (a *Adapter) handleWhoami(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("internal_error", "tether_whoami requires daemon routing; start MCP with tether mcp")
	}
	self, snap, selfErr := a.selfAddress(ctx)
	as := str(args, "as")
	if as == "" {
		if selfErr != nil {
			return nil, selfErr
		}
		as = self
	}
	out, err := a.client.Whoami(ctx, as)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	res := map[string]any{"ok": true, "whoami": out, "principal": snap}
	if self != "" {
		res["address"] = self
		res["inbox"] = self
	}
	return toolJSON(res), nil
}

// selfAddress derives the caller's own inbox address from the daemon-resolved
// session credential: the logical agent's URN, which team rosters and mail
// bind to. It is empty for a caller that is not a resolved session.
func (a *Adapter) selfAddress(ctx context.Context) (string, callcontext.Snapshot, error) {
	snap, err := a.client.CallerContext(ctx)
	if err != nil {
		if isDaemonUnreachable(err) {
			return "", snap, daemonUnreachableError(err)
		}
		return "", snap, mapRegistryErr(err)
	}
	switch {
	case snap.AgentURN != "":
		return snap.AgentURN, snap, nil
	case snap.SessionResolved && snap.LogicalAgentID != "":
		return registry.LogicalAgentBindingTarget(snap.LogicalAgentID), snap, nil
	}
	return "", snap, toolError("invalid_request", "no session address: this caller is not a resolved session; pass as")
}
