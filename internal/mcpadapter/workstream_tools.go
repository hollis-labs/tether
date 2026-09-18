// Package mcpadapter — workstream_tools.go wires the tether_workstream_*
// tools, giving MCP callers parity with /workstreams and
// /sessions/{id}/workstream (internal/api/workstreams.go) and with
// `mux workstreams` on the CLI.
//
// S1 of SP-20260912-0001 (CW-20260912-0059).
package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

// registerWorkstreamTools wires the workstream tool set onto s.
//
// Reads need no scope (same-host UDS trust, ADR 0045). Writes take
// session.write: a workstream is session-lifecycle state, and assigning one
// mutates a session row, which ADR 0035 requires to route through the daemon
// rather than touch the store directly.
func (a *Adapter) registerWorkstreamTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_create",
		Description: "Create a workstream: the durable container for work that outlives any one " +
			"session. A resume/compact/fork child inherits its parent's workstream " +
			"automatically, so a container attached here survives a compaction, which " +
			"creates a new session row and orphans anything keyed on session_id. " +
			"Requires the session.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"name":        strProp("Optional human label."),
			"workflow_id": strProp("Optional correlation id for a workflow owned by another system. Free-form; Tether records it and never resolves it."),
		}),
		Handler: a.handleWorkstreamCreate,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_workstream_get",
		Description: "Fetch one workstream by id.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"id": strProp("Workstream id."),
		}, "id"),
		Handler: a.handleWorkstreamGet,
	}, Reads("store.GetWorkstream: single SELECT"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_workstream_list",
		Description: "List workstreams, newest first.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"status":      strProp("Filter by status: active or closed."),
			"workflow_id": strProp("Filter by workflow correlation id."),
		}),
		Handler: a.handleWorkstreamList,
	}, Reads("store.ListWorkstreams: SELECT"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_assign",
		Description: "Assign a session to a workstream, or clear it by passing an empty " +
			"workstream_id. Requires the session.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id":    strProp("Session to stamp."),
			"workstream_id": strProp("Workstream to assign; empty clears the association."),
		}, "session_id"),
		Handler: a.handleWorkstreamAssign,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_ensure",
		Description: "Return the workstream containing a session, creating one for the whole " +
			"lineage when it has none. This is the one-call path for a session that " +
			"needs a container without ceremony. Idempotent: a second call returns the " +
			"same workstream. Requires the session.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id":  strProp("Session that needs a container."),
			"name":        strProp("Optional label, used only when one is created."),
			"workflow_id": strProp("Optional workflow correlation id, used only when one is created."),
		}, "session_id"),
		Handler: a.handleWorkstreamEnsure,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_namespace",
		Description: "Resolve where a session's workstream-scoped scratch belongs in Tesseract workspace. " +
			"Write the content to Tesseract yourself (with workstream_id as an attribute) " +
			"and attach the returned item or revision id with tether_workstream_attach — " +
			"Tether stores the location and references, never the content.\n\n" +
			"Workstream ID is an attribute, never a namespace path segment. Material lives in " +
			"Tesseract's workspace domain: project-owned scratch under project/<project-id>/workspace/scratch " +
			"or Tether's cross-project scratch under app/tether/workspace/scratch.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("The session asking. Its workstream is resolved for you."),
			"project":    strProp("Declared project identifier (e.g. tether). Defaults to the session's declared project_id."),
			"owner":      strProp("Explicit scope head (e.g. app/tether)."),
			"tail":       strProp("Workspace segment (defaults to scratch)."),
			"user":       strProp("Optional legacy Tesseract user id."),
			"type":       strProp("Optional legacy memory type, mapped to tail if provided."),
		}, "session_id"),
		Handler: a.handleWorkstreamNamespace,
	}, Reads("store.SessionWorkstreamNamespace resolves and deliberately does not auto-create a workstream"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_digest",
		Description: "What a session, or a whole workstream across its lineage, actually touched " +
			"and actually left behind. This is the recovery view: hand the output to an " +
			"agent as context rather than reading a transcript.\n\n" +
			"Pass session_id for the everyday grain (what did I touch this session) or " +
			"workstream_id for the roll-up across a compaction. Exactly one.\n\n" +
			"Refs are split into left_behind (created/updated -- what this work produced) " +
			"and touched (read/referenced -- what it consulted). Read left_behind first; " +
			"it is what a reviewer and a recovery instruction both care about.\n\n" +
			"BEFORE CONCLUDING A SESSION DID NOTHING, read coverage. Every session in the " +
			"span carries a ref_attribution saying whether its proxy could produce an " +
			"observed ref at all, and coverage.proxy_attributable counts how many could. " +
			"When that is zero, an empty source=proxy column is a fact about " +
			"configuration and says nothing about what the agent did. coverage.truncated " +
			"says whether the ref list was cut at the limit.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id":    strProp("Session to digest. Its own refs only; the response carries its workstream so you can escalate to the roll-up."),
			"workstream_id": strProp("Workstream to digest. Rolls up every session in the container -- the grain that survives a compaction."),
			"kind":          strProp("Filter to one ref kind: torque_task, tesseract_revision, git_commit, ..."),
			"relation":      strProp("Filter to one relation: created, updated, read, referenced."),
			"source":        strProp("Filter to one source: proxy (observed by the proxy), api, or agent (self-asserted). proxy means OBSERVED, never validated."),
			"since":         strProp("RFC3339 UTC lower bound on a ref's timestamp. Ask what was in flight rather than everything ever."),
			"limit":         numProp("Maximum refs to return; the response reports whether it truncated."),
		}),
		Handler: a.handleWorkstreamDigest,
	}, Reads("store.SessionDigest / WorkstreamDigest: SELECT only"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_workstreams_for_ref",
		Description: "Which workstreams contain a session that touched this object -- the reverse " +
			"lookup. Use it when you hold an identifier and want the work it came out of: " +
			"\"which Tesseract records came from CW-20260911-0039?\" starts here, then " +
			"digests the result.\n\n" +
			"RETURNS ALL MATCHES AND NEVER PICKS ONE. Two separate efforts touching the " +
			"same task is ordinary, so a single answer would look authoritative and be " +
			"wrong whenever the ambiguity is real.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"ref": strProp("Selector as <kind>:<ref_id>, for example torque_task:CW-20260912-0063. Split on the FIRST colon only, so a ref_id containing colons (msg://...) is preserved."),
		}, "ref"),
		Handler: a.handleWorkstreamsForRef,
	}, Reads("store.WorkstreamsForRef: one SELECT DISTINCT"))
}

func (a *Adapter) handleWorkstreamDigest(ctx context.Context, args map[string]any) (any, error) {
	sessionID, workstreamID := str(args, "session_id"), str(args, "workstream_id")
	switch {
	case sessionID == "" && workstreamID == "":
		return nil, toolError("invalid_request", "one of session_id or workstream_id is required")
	case sessionID != "" && workstreamID != "":
		// Not a preference to resolve: they are different questions, and
		// silently answering one of them would give a caller who meant the
		// other a plausible wrong answer with no way to notice.
		return nil, toolError("invalid_request", "session_id and workstream_id are mutually exclusive: session grain is this session's own refs, workstream grain rolls up the whole lineage")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	q := client.DigestQuery{
		Kind:     str(args, "kind"),
		Relation: str(args, "relation"),
		Source:   str(args, "source"),
		Since:    str(args, "since"),
		Limit:    intArg(args, "limit", 0),
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
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "digest": out}), nil
}

func (a *Adapter) handleWorkstreamsForRef(ctx context.Context, args map[string]any) (any, error) {
	selector := str(args, "ref")
	if selector == "" {
		return nil, toolError("invalid_request", "ref is required, as <kind>:<ref_id>")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	out, err := a.client.WorkstreamsForRef(ctx, selector)
	if err != nil {
		return nil, workstreamErr(err)
	}
	// Reported rather than left for the caller to count: "which workstream"
	// answered with three is a materially different answer from one, and the
	// count is what makes a consumer notice.
	return toolJSON(map[string]any{"ok": true, "matched": len(out), "workstreams": out}), nil
}

func (a *Adapter) handleWorkstreamNamespace(ctx context.Context, args map[string]any) (any, error) {
	sessionID := str(args, "session_id")
	if sessionID == "" {
		return nil, toolError("invalid_request", "session_id is required")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	opts := client.SessionWorkstreamNamespaceOptions{
		Project: str(args, "project"),
		Owner:   str(args, "owner"),
		Tail:    str(args, "tail"),
		User:    str(args, "user"),
		Type:    str(args, "type"),
	}
	out, err := a.client.SessionWorkstreamNamespace(ctx, sessionID, opts)
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{
		"ok": true, "namespace": out.Namespace, "workstream_id": out.WorkstreamID,
	}), nil
}

// workstreamClientReady guards the daemon-routing precondition shared by
// every handler here. Returns a tool error when the adapter has no daemon
// client.
func (a *Adapter) workstreamClientReady() error {
	if a.client == nil {
		return toolError("internal_error", "workstream tools require daemon routing; start MCP with mux mcp")
	}
	return nil
}

func (a *Adapter) handleWorkstreamCreate(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope("session.write"); err != nil {
		return nil, err
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	out, err := a.client.CreateWorkstream(ctx, str(args, "name"), str(args, "workflow_id"))
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

func (a *Adapter) handleWorkstreamGet(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "id")
	if id == "" {
		return nil, toolError("invalid_request", "id is required")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	out, err := a.client.GetWorkstream(ctx, id)
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

func (a *Adapter) handleWorkstreamList(ctx context.Context, args map[string]any) (any, error) {
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	out, err := a.client.ListWorkstreams(ctx, str(args, "status"), str(args, "workflow_id"))
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "workstreams": out}), nil
}

func (a *Adapter) handleWorkstreamAssign(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope("session.write"); err != nil {
		return nil, err
	}
	sessionID := str(args, "session_id")
	if sessionID == "" {
		return nil, toolError("invalid_request", "session_id is required")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	if err := a.client.AssignSessionWorkstream(ctx, sessionID, str(args, "workstream_id")); err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleWorkstreamEnsure(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope("session.write"); err != nil {
		return nil, err
	}
	sessionID := str(args, "session_id")
	if sessionID == "" {
		return nil, toolError("invalid_request", "session_id is required")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	out, err := a.client.EnsureSessionWorkstream(ctx, sessionID, str(args, "name"), str(args, "workflow_id"))
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "workstream": out}), nil
}

// workstreamErr maps a client error onto the canonical tool-error shape,
// preserving the daemon-unreachable case that every other daemon-routed tool
// reports distinctly.
func workstreamErr(err error) error {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return toolError("internal_error", err.Error())
}
