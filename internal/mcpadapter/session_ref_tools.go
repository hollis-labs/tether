// Package mcpadapter — session_ref_tools.go wires tether_workstream_attach and
// tether_session_refs: what a session touched, and the workstream roll-up that
// survives a compaction.
//
// S2 of SP-20260912-0001 (CW-20260912-0060).
package mcpadapter

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/api"
)

// registerSessionRefTools wires the ref tool set onto s.
//
// The attach tool is the surface the commit / PR / end-session hooks call: git
// is out of the proxy's reach entirely (commits happen via Bash, which never
// reaches Tether), so those refs arrive by explicit capture or not at all.
func (a *Adapter) registerSessionRefTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_workstream_attach",
		mcp.WithDescription(
			"Record that a session touched an object: a Torque task, a git commit or PR, "+
				"a Tesseract revision, a Cerberus deploy, an ADR, a URL. Idempotent -- "+
				"re-attaching the same (kind, ref_id, relation) is a no-op, not an error, so "+
				"a hook that runs twice is safe. A repeat does NOT revise the original: the "+
				"row records what was asserted at the time. Refs attach to the session and "+
				"roll up through its workstream, so they survive a compaction. Requires the "+
				"session.write scope.",
		),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session that touched the object.")),
		mcp.WithString("kind", mcp.Required(), mcp.Description("What sort of object: torque_task, git_commit, git_pr, tesseract_revision, cerberus_deploy, adr, url.")),
		mcp.WithString("ref_id", mcp.Required(), mcp.Description("The identifier, e.g. CW-20260912-0023 or 37b4dc0.")),
		mcp.WithString("uri", mcp.Description("Optional resolvable locator.")),
		mcp.WithString("relation", mcp.Description("created | updated | read | referenced. Defaults to referenced. This is what makes the record answer a question: reading a task and creating one are both 'touched', but only one is 'left behind'.")),
	), a.handleWorkstreamAttach)

	a.addTool(s, mcp.NewTool("tether_session_refs",
		mcp.WithDescription(
			"List what a session touched, or -- with workstream_id instead -- everything "+
				"every session in a workstream touched. The workstream form is the one that "+
				"survives a compaction, since a compaction creates a new session row.\n\n"+
				"Reading `source`: 'proxy' means the proxy OBSERVED this session make this "+
				"call, which the calling agent cannot fabricate. It does NOT mean the "+
				"identifier was validated -- nothing checks that the id refers to anything. "+
				"And absence is not evidence: direct MCP children, HTTP callers and the CLI "+
				"all bypass the proxy, so a session that did real work through those paths "+
				"produces no proxy-observed refs. That is normal, not suspicious.",
		),
		mcp.WithString("session_id", mcp.Description("List one session's refs. Provide this or workstream_id.")),
		mcp.WithString("workstream_id", mcp.Description("List the whole workstream roll-up. Provide this or session_id.")),
		mcp.WithString("kind", mcp.Description("Filter by kind.")),
		mcp.WithString("relation", mcp.Description("Filter by relation.")),
		mcp.WithString("source", mcp.Description("Filter by source: proxy, api or agent.")),
	), a.handleSessionRefs)
}

func (a *Adapter) handleWorkstreamAttach(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope("session.write"); errRes != nil {
		return errRes, nil
	}
	sessionID := str(req, "session_id")
	kind := str(req, "kind")
	refID := str(req, "ref_id")
	switch {
	case sessionID == "":
		return toolError("invalid_request", "session_id is required"), nil
	case kind == "":
		return toolError("invalid_request", "kind is required"), nil
	case refID == "":
		return toolError("invalid_request", "ref_id is required"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	// source is not exposed as a parameter: an agent calling this tool IS the
	// asserting party, so the honest value is "agent" and letting the caller
	// name it would erase the only distinction the column carries.
	out, err := a.client.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{
		Kind:     kind,
		RefID:    refID,
		URI:      str(req, "uri"),
		Relation: str(req, "relation"),
		Source:   "agent",
	})
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok": true, "inserted": out.Inserted, "upgraded": out.Upgraded, "ref": out.Ref,
	}), nil
}

func (a *Adapter) handleSessionRefs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := str(req, "session_id")
	workstreamID := str(req, "workstream_id")
	switch {
	case sessionID == "" && workstreamID == "":
		return toolError("invalid_request", "provide session_id or workstream_id"), nil
	case sessionID != "" && workstreamID != "":
		return toolError("invalid_request", "provide session_id or workstream_id, not both"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	kind, relation, source := str(req, "kind"), str(req, "relation"), str(req, "source")

	var (
		refs []api.SessionRefDTO
		err  error
	)
	if workstreamID != "" {
		refs, err = a.client.ListWorkstreamRefs(ctx, workstreamID, kind, relation, source)
	} else {
		refs, err = a.client.ListSessionRefs(ctx, sessionID, kind, relation, source)
	}
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "refs": refs}), nil
}
