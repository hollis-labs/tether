// Package mcpadapter — workstream_tools.go wires the tether_workstream_*
// tools, giving MCP callers parity with /workstreams and
// /sessions/{id}/workstream (internal/api/workstreams.go) and with
// `mux workstreams` on the CLI.
//
// S1 of SP-20260912-0001 (CW-20260912-0059).
package mcpadapter

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

// registerWorkstreamTools wires the workstream tool set onto s.
//
// Reads need no scope (same-host UDS trust, ADR 0045). Writes take
// session.write: a workstream is session-lifecycle state, and assigning one
// mutates a session row, which ADR 0035 requires to route through the daemon
// rather than touch the store directly.
func (a *Adapter) registerWorkstreamTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_workstream_create",
		mcp.WithDescription(
			"Create a workstream: the durable container for work that outlives any one "+
				"session. A resume/compact/fork child inherits its parent's workstream "+
				"automatically, so a container attached here survives a compaction, which "+
				"creates a new session row and orphans anything keyed on session_id. "+
				"Requires the session.write scope.",
		),
		mcp.WithString("name", mcp.Description("Optional human label.")),
		mcp.WithString("workflow_id", mcp.Description("Optional correlation id for a workflow owned by another system. Free-form; Tether records it and never resolves it.")),
	), a.handleWorkstreamCreate)

	a.addTool(s, mcp.NewTool("tether_workstream_get",
		mcp.WithDescription("Fetch one workstream by id."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Workstream id.")),
	), a.handleWorkstreamGet)

	a.addTool(s, mcp.NewTool("tether_workstream_list",
		mcp.WithDescription("List workstreams, newest first."),
		mcp.WithString("status", mcp.Description("Filter by status: active or closed.")),
		mcp.WithString("workflow_id", mcp.Description("Filter by workflow correlation id.")),
	), a.handleWorkstreamList)

	a.addTool(s, mcp.NewTool("tether_workstream_assign",
		mcp.WithDescription(
			"Assign a session to a workstream, or clear it by passing an empty "+
				"workstream_id. Requires the session.write scope.",
		),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session to stamp.")),
		mcp.WithString("workstream_id", mcp.Description("Workstream to assign; empty clears the association.")),
	), a.handleWorkstreamAssign)

	a.addTool(s, mcp.NewTool("tether_workstream_ensure",
		mcp.WithDescription(
			"Return the workstream containing a session, creating one for the whole "+
				"lineage when it has none. This is the one-call path for a session that "+
				"needs a container without ceremony. Idempotent: a second call returns the "+
				"same workstream. Requires the session.write scope.",
		),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session that needs a container.")),
		mcp.WithString("name", mcp.Description("Optional label, used only when one is created.")),
		mcp.WithString("workflow_id", mcp.Description("Optional workflow correlation id, used only when one is created.")),
	), a.handleWorkstreamEnsure)

	a.addTool(s, mcp.NewTool("tether_workstream_namespace",
		mcp.WithDescription(
			"Resolve where a session's workstream-scoped scratch, notes and todos belong "+
				"in Tesseract. Write the content to Tesseract yourself and attach the "+
				"returned revision id with tether_workstream_attach — Tether stores the "+
				"location, never the content.\n\n"+
				"The namespace keys on the WORKSTREAM, not the session, which is what makes "+
				"a note written before a compaction readable after one: a compaction creates "+
				"a new session row, and anything keyed on a session id is orphaned by it.\n\n"+
				"The id in the session segment is prefixed ws_ and is a workstream id, NOT a "+
				"session id. Do not try to correlate it against a session — it will never "+
				"match, which is the point: a bare id there would fail silently instead.",
		),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("The session asking. Its workstream is resolved for you.")),
		mcp.WithString("user", mcp.Required(), mcp.Description("Tesseract user id; Tether does not own that identity and will not invent one.")),
		mcp.WithString("type", mcp.Description("Tesseract memory type: notes, todos, decisions, ... Defaults to notes. Passed through unvalidated — the vocabulary is Tesseract's.")),
	), a.handleWorkstreamNamespace)

	a.addTool(s, mcp.NewTool("tether_workstream_digest",
		mcp.WithDescription(
			"What a session, or a whole workstream across its lineage, actually touched "+
				"and actually left behind. This is the recovery view: hand the output to an "+
				"agent as context rather than reading a transcript.\n\n"+
				"Pass session_id for the everyday grain (what did I touch this session) or "+
				"workstream_id for the roll-up across a compaction. Exactly one.\n\n"+
				"Refs are split into left_behind (created/updated -- what this work produced) "+
				"and touched (read/referenced -- what it consulted). Read left_behind first; "+
				"it is what a reviewer and a recovery instruction both care about.\n\n"+
				"BEFORE CONCLUDING A SESSION DID NOTHING, read coverage. Every session in the "+
				"span carries a ref_attribution saying whether its proxy could produce an "+
				"observed ref at all, and coverage.proxy_attributable counts how many could. "+
				"When that is zero, an empty source=proxy column is a fact about "+
				"configuration and says nothing about what the agent did. coverage.truncated "+
				"says whether the ref list was cut at the limit.",
		),
		mcp.WithString("session_id", mcp.Description("Session to digest. Its own refs only; the response carries its workstream so you can escalate to the roll-up.")),
		mcp.WithString("workstream_id", mcp.Description("Workstream to digest. Rolls up every session in the container -- the grain that survives a compaction.")),
		mcp.WithString("kind", mcp.Description("Filter to one ref kind: torque_task, tesseract_revision, git_commit, ...")),
		mcp.WithString("relation", mcp.Description("Filter to one relation: created, updated, read, referenced.")),
		mcp.WithString("source", mcp.Description("Filter to one source: proxy (observed by the proxy), api, or agent (self-asserted). proxy means OBSERVED, never validated.")),
		mcp.WithString("since", mcp.Description("RFC3339 UTC lower bound on a ref's timestamp. Ask what was in flight rather than everything ever.")),
		mcp.WithNumber("limit", mcp.Description("Maximum refs to return; the response reports whether it truncated.")),
	), a.handleWorkstreamDigest)

	a.addTool(s, mcp.NewTool("tether_workstreams_for_ref",
		mcp.WithDescription(
			"Which workstreams contain a session that touched this object -- the reverse "+
				"lookup. Use it when you hold an identifier and want the work it came out of: "+
				"\"which Tesseract records came from CW-20260911-0039?\" starts here, then "+
				"digests the result.\n\n"+
				"RETURNS ALL MATCHES AND NEVER PICKS ONE. Two separate efforts touching the "+
				"same task is ordinary, so a single answer would look authoritative and be "+
				"wrong whenever the ambiguity is real.",
		),
		mcp.WithString("ref", mcp.Required(), mcp.Description("Selector as <kind>:<ref_id>, for example torque_task:CW-20260912-0063. Split on the FIRST colon only, so a ref_id containing colons (msg://...) is preserved.")),
	), a.handleWorkstreamsForRef)
}

func (a *Adapter) handleWorkstreamDigest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, workstreamID := str(req, "session_id"), str(req, "workstream_id")
	switch {
	case sessionID == "" && workstreamID == "":
		return toolError("invalid_request", "one of session_id or workstream_id is required"), nil
	case sessionID != "" && workstreamID != "":
		// Not a preference to resolve: they are different questions, and
		// silently answering one of them would give a caller who meant the
		// other a plausible wrong answer with no way to notice.
		return toolError("invalid_request", "session_id and workstream_id are mutually exclusive: session grain is this session's own refs, workstream grain rolls up the whole lineage"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	q := client.DigestQuery{
		Kind:     str(req, "kind"),
		Relation: str(req, "relation"),
		Source:   str(req, "source"),
		Since:    str(req, "since"),
		Limit:    intArg(req, "limit", 0),
	}
	var (
		out api.DigestResponse
		err error
	)
	if sessionID != "" {
		out, err = a.client.SessionDigest(ctx, sessionID, q)
	} else {
		out, err = a.client.WorkstreamDigest(ctx, workstreamID, q)
	}
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "digest": out}), nil
}

func (a *Adapter) handleWorkstreamsForRef(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	selector := str(req, "ref")
	if selector == "" {
		return toolError("invalid_request", "ref is required, as <kind>:<ref_id>"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.WorkstreamsForRef(ctx, selector)
	if err != nil {
		return workstreamErr(err), nil
	}
	// Reported rather than left for the caller to count: "which workstream"
	// answered with three is a materially different answer from one, and the
	// count is what makes a consumer notice.
	return toolJSON(map[string]any{"ok": true, "matched": len(out), "workstreams": out}), nil
}

func (a *Adapter) handleWorkstreamNamespace(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := str(req, "session_id")
	if sessionID == "" {
		return toolError("invalid_request", "session_id is required"), nil
	}
	user := str(req, "user")
	if user == "" {
		return toolError("invalid_request", "user is required: Tether does not own the Tesseract user identity"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.SessionWorkstreamNamespace(ctx, sessionID, user, str(req, "type"))
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok": true, "namespace": out.Namespace, "workstream_id": out.WorkstreamID,
	}), nil
}

// workstreamClient guards the daemon-routing precondition shared by every
// handler here. Returns a tool error when the adapter has no daemon client.
func (a *Adapter) workstreamClientReady() *mcp.CallToolResult {
	if a.client == nil {
		return toolError("internal_error", "workstream tools require daemon routing; start MCP with mux mcp")
	}
	return nil
}

func (a *Adapter) handleWorkstreamCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope("session.write"); errRes != nil {
		return errRes, nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.CreateWorkstream(ctx, str(req, "name"), str(req, "workflow_id"))
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

func (a *Adapter) handleWorkstreamGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "id")
	if id == "" {
		return toolError("invalid_request", "id is required"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.GetWorkstream(ctx, id)
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

func (a *Adapter) handleWorkstreamList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.ListWorkstreams(ctx, str(req, "status"), str(req, "workflow_id"))
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "workstreams": out}), nil
}

func (a *Adapter) handleWorkstreamAssign(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope("session.write"); errRes != nil {
		return errRes, nil
	}
	sessionID := str(req, "session_id")
	if sessionID == "" {
		return toolError("invalid_request", "session_id is required"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	if err := a.client.AssignSessionWorkstream(ctx, sessionID, str(req, "workstream_id")); err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleWorkstreamEnsure(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope("session.write"); errRes != nil {
		return errRes, nil
	}
	sessionID := str(req, "session_id")
	if sessionID == "" {
		return toolError("invalid_request", "session_id is required"), nil
	}
	if errRes := a.workstreamClientReady(); errRes != nil {
		return errRes, nil
	}
	out, err := a.client.EnsureSessionWorkstream(ctx, sessionID, str(req, "name"), str(req, "workflow_id"))
	if err != nil {
		return workstreamErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

// workstreamErr maps a client error onto the canonical tool-error shape,
// preserving the daemon-unreachable case that every other daemon-routed tool
// reports distinctly.
func workstreamErr(err error) *mcp.CallToolResult {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return toolError("internal_error", err.Error())
}
