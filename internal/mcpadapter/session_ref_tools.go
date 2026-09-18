// Package mcpadapter — session_ref_tools.go wires tether_workstream_attach and
// tether_session_refs: what a session touched, and the workstream roll-up that
// survives a compaction.
//
// S2 of SP-20260912-0001 (CW-20260912-0060).
package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/api"
)

// registerSessionRefTools wires the ref tool set onto s.
//
// The attach tool is the surface the commit / PR / end-session hooks call: git
// is out of the proxy's reach entirely (commits happen via Bash, which never
// reaches Tether), so those refs arrive by explicit capture or not at all.
func (a *Adapter) registerSessionRefTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_workstream_attach",
		Description: "Record that a session touched an object: a Torque task, a git commit or PR, " +
			"a Tesseract revision, a Cerberus deploy, an ADR, a URL. Idempotent -- " +
			"re-attaching the same (kind, ref_id, relation) is a no-op, not an error, so " +
			"a hook that runs twice is safe. A repeat does NOT revise the original: the " +
			"row records what was asserted at the time. Refs attach to the session and " +
			"roll up through its workstream, so they survive a compaction. Requires the " +
			"session.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session that touched the object."),
			"kind":       strProp("What sort of object: torque_task, git_commit, git_pr, tesseract_revision, cerberus_deploy, adr, url."),
			"ref_id":     strProp("The identifier, e.g. CW-20260912-0023 or 37b4dc0."),
			"uri":        strProp("Optional resolvable locator."),
			"relation":   strProp("created | updated | read | referenced. Defaults to referenced. This is what makes the record answer a question: reading a task and creating one are both 'touched', but only one is 'left behind'."),
		}, "session_id", "kind", "ref_id"),
		Handler: a.handleWorkstreamAttach,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_session_refs",
		Description: "List what a session touched, or -- with workstream_id instead -- everything " +
			"every session in a workstream touched. The workstream form is the one that " +
			"survives a compaction, since a compaction creates a new session row.\n\n" +
			"Reading `source`: 'proxy' means the proxy OBSERVED this session make this " +
			"call, which the calling agent cannot fabricate. It does NOT mean the " +
			"identifier was validated -- nothing checks that the id refers to anything. " +
			"And absence is not evidence: direct MCP children, HTTP callers and the CLI " +
			"all bypass the proxy, so a session that did real work through those paths " +
			"produces no proxy-observed refs. That is normal, not suspicious.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id":    strProp("List one session's refs. Provide this or workstream_id."),
			"workstream_id": strProp("List the whole workstream roll-up. Provide this or session_id."),
			"kind":          strProp("Filter by kind."),
			"relation":      strProp("Filter by relation."),
			"source":        strProp("Filter by source: proxy, api or agent."),
		}),
		Handler: a.handleSessionRefs,
	}, Reads("store.ListSessionRefs / ListWorkstreamRefs: SELECT"))
}

func (a *Adapter) handleWorkstreamAttach(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope("session.write"); err != nil {
		return nil, err
	}
	sessionID := str(args, "session_id")
	kind := str(args, "kind")
	refID := str(args, "ref_id")
	switch {
	case sessionID == "":
		return nil, toolError("invalid_request", "session_id is required")
	case kind == "":
		return nil, toolError("invalid_request", "kind is required")
	case refID == "":
		return nil, toolError("invalid_request", "ref_id is required")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	// source is not exposed as a parameter: an agent calling this tool IS the
	// asserting party, so the honest value is "agent" and letting the caller
	// name it would erase the only distinction the column carries.
	out, err := a.client.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{
		Kind:         kind,
		RefID:        refID,
		URI:          str(args, "uri"),
		Relation:     str(args, "relation"),
		Source:       "agent",
		ParentItemID: str(args, "parent_item_id"),
	})
	if err != nil {
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{
		"ok": true, "inserted": out.Inserted, "upgraded": out.Upgraded, "ref": out.Ref,
	}), nil
}

func (a *Adapter) handleSessionRefs(ctx context.Context, args map[string]any) (any, error) {
	sessionID := str(args, "session_id")
	workstreamID := str(args, "workstream_id")
	switch {
	case sessionID == "" && workstreamID == "":
		return nil, toolError("invalid_request", "provide session_id or workstream_id")
	case sessionID != "" && workstreamID != "":
		return nil, toolError("invalid_request", "provide session_id or workstream_id, not both")
	}
	if err := a.workstreamClientReady(); err != nil {
		return nil, err
	}
	kind, relation, source := str(args, "kind"), str(args, "relation"), str(args, "source")

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
		return nil, workstreamErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "refs": refs}), nil
}
