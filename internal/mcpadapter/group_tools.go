// Package mcpadapter — group_tools.go wires the native tether_group_*
// MCP tools (T-v060-05-06, converted to daemon routing in T08 messaging
// vNext). One tool per HTTP endpoint in internal/api/groups.go; every
// handler now routes through a.client (the daemon HTTP client) rather
// than dispatching to a.svc.Registry in-process — see registry_tools.go's
// package doc for why (the `mux mcp` split-brain SQLite-connection issue
// T05 fixed for messages, applied here too).
//
// Scope. Membership + post + read tools require `groups.write`. Read
// tools (lookup, list-for-member, list-members, list-messages,
// mentions) are unauthenticated (same-host UDS trust, D7 in v060-01).
//
// Symbol-vocabulary documentation. The tether_group_post tool
// description is the v1 source-of-truth for explaining to agent
// authors which symbols the daemon parses (only `@`) vs which are
// reserved-namespace for agent-side handling (`!` and `:`). The text
// is duplicated rather than cross-referenced because agent tool
// catalogs often render descriptions inline without following links.
// Keep the explanation crisp; defer the directives-package details to
// the symbol-vocabulary doc.
//
// Error mapping (mirrors HTTP surface):
//
//	registry.ErrInvalidRequest       → "invalid_request"
//	*registry.ErrAmbiguousMention    → "invalid_request" + candidates
//	registry.ErrForbidden            → "forbidden"
//	registry.ErrNotFound             → "not_found"
//	registry.ErrGroupArchived        → "locked"
//	(anything else)                  → "internal_error"

package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
)

// ScopeGroupsWrite gates the mutating group tools (create, invite,
// kick, set_role, leave, post, mark_read, archive). Read tools require
// no scope per the same-host UDS trust model.
const ScopeGroupsWrite = "groups.write"

// registerGroupTools wires every tether_group_* native tool onto s.
// Mirrors registerRegistryTools — every handler routes through a.client
// to the daemon's /groups HTTP surface.
func (a *Adapter) registerGroupTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_group_create",
		mcp.WithDescription(
			"Create a new group (federation directory kind=group). The server mints "+
				"the URN — 3-segment msg://group/<authority>/grp_<10alnum>. The creator is "+
				"automatically added to the group with role='owner'. "+
				"The display_name is required; description, role (group category), and "+
				"capabilities (topic tags) are optional. The creator URN is supplied via "+
				"'creator_urn' (auth surrogate for v060-05; v060-03 token auth will replace). "+
				"Requires the groups.write scope.",
		),
		mcp.WithString("display_name", mcp.Required(),
			mcp.Description("Human-readable group name."),
		),
		mcp.WithString("creator_urn", mcp.Required(),
			mcp.Description("Caller URN; must exist as an active agent/project registry row. Becomes the owner."),
		),
		mcp.WithString("description",
			mcp.Description("Free-form description of the group's purpose."),
		),
		mcp.WithString("role",
			mcp.Description("Group category (free-form), e.g. 'design-room' / 'incident-bridge' / 'project-coord'."),
		),
		mcp.WithArray("capabilities",
			mcp.Description("Topic tags for discovery via tether_registry_search."),
		),
	), a.handleGroupCreate)

	a.addTool(s, mcp.NewTool("tether_group_lookup",
		mcp.WithDescription(
			"Look up a group by URN. Returns the full Profile. The 'urn' must be a "+
				"group URN (msg://group/...); agent URNs return not_found. Archived "+
				"groups are still returned (callers may need their metadata). Read-only.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full group URN, e.g. msg://group/agent-mux/grp_xxxxxxxxxx."),
		),
	), a.handleGroupLookup)

	a.addTool(s, mcp.NewTool("tether_group_list_for_member",
		mcp.WithDescription(
			"List the groups a member belongs to. Returns active + archived groups "+
				"(archived ones are still visible to former members). Read-only.",
		),
		mcp.WithString("member_urn", mcp.Required(),
			mcp.Description("Full URN of the member whose group list we're fetching."),
		),
	), a.handleGroupListForMember)

	a.addTool(s, mcp.NewTool("tether_group_archive",
		mcp.WithDescription(
			"Soft-delete a group (sets status='archived'). The group becomes read-only "+
				"— members can still read history, but no new messages are accepted. "+
				"Only the owner or a moderator can archive. Requires the groups.write scope.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full group URN to archive."),
		),
		mcp.WithString("by", mcp.Required(),
			mcp.Description("Caller URN (must be the group's owner or a moderator)."),
		),
	), a.handleGroupArchive)

	a.addTool(s, mcp.NewTool("tether_group_invite",
		mcp.WithDescription(
			"Add a member to a group. Only owners and moderators can invite. The "+
				"member URN must exist in the registry. Role defaults to 'member' if "+
				"unset. Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("member_urn", mcp.Required(),
			mcp.Description("Full URN of the member being invited (must exist in registry)."),
		),
		mcp.WithString("by", mcp.Required(),
			mcp.Description("Caller URN (must be owner or moderator)."),
		),
		mcp.WithString("role",
			mcp.Description("Role at invite time: 'member' (default) | 'moderator'."),
			mcp.Enum("member", "moderator"),
		),
	), a.handleGroupInvite)

	a.addTool(s, mcp.NewTool("tether_group_kick",
		mcp.WithDescription(
			"Remove a member from a group. Only owners and moderators can kick. The "+
				"group's owner cannot be removed by this tool — the owner must use "+
				"tether_group_leave after transferring ownership via tether_group_set_role. "+
				"Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("member_urn", mcp.Required(),
			mcp.Description("Full URN of the member being kicked."),
		),
		mcp.WithString("by", mcp.Required(),
			mcp.Description("Caller URN (must be owner or moderator)."),
		),
	), a.handleGroupKick)

	a.addTool(s, mcp.NewTool("tether_group_leave",
		mcp.WithDescription(
			"Self-remove from a group. If the leaver is the group's owner, another "+
				"member must already hold owner or moderator role — otherwise the leave "+
				"is refused with cannot_leave_without_owner_transfer (forbidden). Use "+
				"tether_group_set_role to promote first. Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("member_urn", mcp.Required(),
			mcp.Description("Caller URN (the leaver — must equal the caller's own identity)."),
		),
	), a.handleGroupLeave)

	a.addTool(s, mcp.NewTool("tether_group_set_role",
		mcp.WithDescription(
			"Change a member's role within a group. Promotion to 'owner' is owner-only "+
				"(transfers ownership). Moderators can promote to 'moderator' but not to "+
				"'owner'. Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("member_urn", mcp.Required(),
			mcp.Description("Full URN of the member whose role is changing."),
		),
		mcp.WithString("role", mcp.Required(),
			mcp.Description("New role: 'member' | 'moderator' | 'owner'."),
			mcp.Enum("member", "moderator", "owner"),
		),
		mcp.WithString("by", mcp.Required(),
			mcp.Description("Caller URN (must be owner or moderator; only owner can promote to owner)."),
		),
	), a.handleGroupSetRole)

	a.addTool(s, mcp.NewTool("tether_group_list_members",
		mcp.WithDescription(
			"List the members of a group, ordered by joined_at. Display names are "+
				"hydrated from the registry. Read-only.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
	), a.handleGroupListMembers)

	a.addTool(s, mcp.NewTool("tether_group_post",
		mcp.WithDescription(
			"Post a message to a group. The caller (from_urn) must be a member; the "+
				"group must not be archived. Returns {message_id, group_seq}. "+
				"\n\n"+
				"SYMBOL VOCABULARY (v060-05 D6 — important):\n"+
				"  - '@<urn>' or '@<display_name>' = MENTION. The daemon parses the "+
				"body before commit, resolves each token via the registry, and emits a "+
				"notice envelope to the mentioned URN's personal inbox after the group "+
				"message commits. Short-form ('@<display_name>') resolves via registry "+
				"Lookup; ambiguous (two members share the same display_name) returns "+
				"invalid_request with a 'candidates' list — re-issue with the full URN.\n"+
				"  - '!<command> [args...]' = ACTION/COMMAND. RESERVED NAMESPACE — the "+
				"daemon does NOT parse '!'. The transport delivers '!' bytes verbatim; "+
				"consuming agents decide what '!deploy --branch=main' means in their "+
				"command handler. Reserved so future tooling (linters, audit log "+
				"filters) can rely on the convention.\n"+
				"  - ':<directive> <prompt>' = DIRECTIVE. RESERVED NAMESPACE — the "+
				"daemon does NOT parse ':'. The consuming agent routes the directive "+
				"through whatever directives-package implementation it has installed "+
				"locally. The actual directive vocabulary, security model, and "+
				"dispatch mechanics are owned by the directives package, not this "+
				"sprint.\n"+
				"  - To send any of these symbols literally, escape with '\\@', "+
				"'\\!', or '\\:' — the daemon never resolves an escaped form. Useful "+
				"for documentation, examples, and quoted snippets.\n"+
				"\n"+
				"Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN to post into."),
		),
		mcp.WithString("from_urn", mcp.Required(),
			mcp.Description("Caller URN (must be a member of the group)."),
		),
		mcp.WithString("kind",
			mcp.Description("Envelope kind. Defaults to 'message'. Other valid values match the messaging-store kind vocabulary."),
		),
		mcp.WithString("thread_id",
			mcp.Description("Optional thread id — groups subdivide into threads via this field (no hierarchical URN)."),
		),
		mcp.WithString("content_type",
			mcp.Description("MIME-ish content type for the payload (e.g. 'text/plain', 'application/json')."),
		),
		mcp.WithObject("payload", mcp.Required(),
			mcp.Description("Envelope payload as a JSON object. The daemon-side mention parser scans this for '@' tokens."),
		),
	), a.handleGroupPost)

	a.addTool(s, mcp.NewTool("tether_group_read",
		mcp.WithDescription(
			"Non-destructive read of group messages. Does NOT bump the read cursor — "+
				"call tether_group_mark_read after acknowledging the batch. Default "+
				"since_seq is the caller's last_read_seq (0 → from cursor). "+
				"thread_id filters to one sub-conversation. Read-only.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("as", mcp.Required(),
			mcp.Description("Caller URN (must be a member; identity surrogate per v060-05)."),
		),
		mcp.WithNumber("since_seq",
			mcp.Description("Lower bound on group_seq (exclusive). 0 → use caller's last_read_seq."),
		),
		mcp.WithString("thread_id",
			mcp.Description("Optional thread id filter."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max messages to return. Server default: 100."),
		),
	), a.handleGroupRead)

	a.addTool(s, mcp.NewTool("tether_group_mark_read",
		mcp.WithDescription(
			"Bump the caller's read cursor for a group. Idempotent / monotonic — "+
				"smaller up_to_seq is silently a no-op. Requires the groups.write scope.",
		),
		mcp.WithString("group_urn", mcp.Required(),
			mcp.Description("Full group URN."),
		),
		mcp.WithString("as", mcp.Required(),
			mcp.Description("Caller URN."),
		),
		mcp.WithNumber("up_to_seq", mcp.Required(),
			mcp.Description("New cursor value — last_read_seq becomes max(last_read_seq, up_to_seq)."),
		),
	), a.handleGroupMarkRead)

	a.addTool(s, mcp.NewTool("tether_group_mentions",
		mcp.WithDescription(
			"List the caller's own mention notices across all groups. Filters by "+
				"timestamp (since) and limit. Read-only.",
		),
		mcp.WithString("as", mcp.Required(),
			mcp.Description("Caller URN — the member whose mentions are being read."),
		),
		mcp.WithString("since",
			mcp.Description("RFC3339 timestamp; mentions emitted after this are returned. Empty → no lower bound."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max mentions to return. Server default: 50."),
		),
	), a.handleGroupMentions)
}

// ─── handlers ────────────────────────────────────────────────────────────

func (a *Adapter) handleGroupCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	name := str(req, "display_name")
	creator := str(req, "creator_urn")
	if name == "" || creator == "" {
		return toolError("invalid_request", "display_name and creator_urn are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_create requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().Create(ctx, client.CreateGroupRequest{
		DisplayName:  name,
		Description:  str(req, "description"),
		Role:         str(req, "role"),
		Capabilities: stringSliceArg(req, "capabilities"),
		CreatorURN:   creator,
	})
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

func (a *Adapter) handleGroupLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_lookup requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().Lookup(ctx, urn)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	if out.Kind != registry.KindGroup {
		return toolError("not_found", fmt.Sprintf("urn %q is not a group (kind=%s)", urn, out.Kind)), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

func (a *Adapter) handleGroupListForMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	member := str(req, "member_urn")
	if member == "" {
		return toolError("invalid_request", "member_urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_list_for_member requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().ListForMember(ctx, member)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	if out == nil {
		out = []registry.Profile{}
	}
	return toolJSON(map[string]any{"ok": true, "groups": out}), nil
}

func (a *Adapter) handleGroupArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	urn := str(req, "urn")
	by := str(req, "by")
	if urn == "" || by == "" {
		return toolError("invalid_request", "urn and by are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_archive requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().Archive(ctx, urn, by)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

func (a *Adapter) handleGroupInvite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	member := str(req, "member_urn")
	by := str(req, "by")
	role := str(req, "role")
	if grp == "" || member == "" || by == "" {
		return toolError("invalid_request", "group_urn, member_urn, and by are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_invite requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().AddMember(ctx, grp, member, by, registry.MemberRole(role))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "member": out}), nil
}

func (a *Adapter) handleGroupKick(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	member := str(req, "member_urn")
	by := str(req, "by")
	if grp == "" || member == "" || by == "" {
		return toolError("invalid_request", "group_urn, member_urn, and by are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_kick requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.Groups().RemoveMember(ctx, grp, member, by); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleGroupLeave(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	member := str(req, "member_urn")
	if grp == "" || member == "" {
		return toolError("invalid_request", "group_urn and member_urn are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_leave requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.Groups().Leave(ctx, grp, member); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleGroupSetRole(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	member := str(req, "member_urn")
	role := str(req, "role")
	by := str(req, "by")
	if grp == "" || member == "" || role == "" || by == "" {
		return toolError("invalid_request", "group_urn, member_urn, role, and by are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_set_role requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.Groups().SetMemberRole(ctx, grp, member, registry.MemberRole(role), by); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleGroupListMembers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	grp := str(req, "group_urn")
	if grp == "" {
		return toolError("invalid_request", "group_urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_list_members requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().ListMembers(ctx, grp)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	if out == nil {
		out = []registry.GroupMember{}
	}
	return toolJSON(map[string]any{"ok": true, "members": out}), nil
}

func (a *Adapter) handleGroupPost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	from := str(req, "from_urn")
	if grp == "" || from == "" {
		return toolError("invalid_request", "group_urn and from_urn are required"), nil
	}
	kind := str(req, "kind")
	threadID := str(req, "thread_id")
	contentType := str(req, "content_type")

	// Re-marshal the payload object so we get a json.RawMessage suitable
	// for the client layer. The same round-trip pattern as
	// decodeProfileArg in registry_tools.go.
	raw, ok := req.GetArguments()["payload"]
	if !ok {
		return toolError("invalid_request", "payload is required"), nil
	}
	pb, mErr := json.Marshal(raw)
	if mErr != nil {
		return toolError("invalid_request", "re-marshal payload: "+mErr.Error()), nil //nolint:nilerr
	}

	if a.client == nil {
		return toolError("internal_error", "tether_group_post requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().Send(ctx, grp, client.SendGroupRequest{
		From:        from,
		Kind:        kind,
		ThreadID:    threadID,
		ContentType: contentType,
		Payload:     pb,
	})
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":         true,
		"message_id": out.MessageID,
		"group_seq":  out.GroupSeq,
	}), nil
}

func (a *Adapter) handleGroupRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	grp := str(req, "group_urn")
	as := str(req, "as")
	if grp == "" || as == "" {
		return toolError("invalid_request", "group_urn and as are required"), nil
	}
	sinceSeq := int64(intArg(req, "since_seq", 0))
	limit := intArg(req, "limit", 100)
	threadID := str(req, "thread_id")

	if a.client == nil {
		return toolError("internal_error", "tether_group_read requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().ListMessages(ctx, grp, client.ListMessagesParams{
		As: as, SinceSeq: sinceSeq, ThreadID: threadID, Limit: limit,
	})
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	messages := out.Messages
	if messages == nil {
		messages = []registry.GroupMessage{}
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"messages": messages,
		"next_seq": out.NextSeq,
	}), nil
}

func (a *Adapter) handleGroupMarkRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeGroupsWrite); errRes != nil {
		return errRes, nil
	}
	grp := str(req, "group_urn")
	as := str(req, "as")
	if grp == "" || as == "" {
		return toolError("invalid_request", "group_urn and as are required"), nil
	}
	upTo := int64(intArg(req, "up_to_seq", -1))
	if upTo < 0 {
		return toolError("invalid_request", "up_to_seq is required and must be ≥ 0"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_mark_read requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.Groups().MarkRead(ctx, grp, as, upTo); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true}), nil
}

func (a *Adapter) handleGroupMentions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	as := str(req, "as")
	if as == "" {
		return toolError("invalid_request", "as is required"), nil
	}
	limit := intArg(req, "limit", 50)
	var since time.Time
	if raw := str(req, "since"); raw != "" {
		t, pErr := time.Parse(time.RFC3339, raw)
		if pErr != nil {
			return toolError("invalid_request", "since must be RFC3339: "+pErr.Error()), nil //nolint:nilerr
		}
		since = t
	}
	if a.client == nil {
		return toolError("internal_error", "tether_group_mentions requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Groups().Mentions(ctx, client.MentionsParams{As: as, Since: since, Limit: limit})
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapGroupErr(err), nil
	}
	if out == nil {
		out = []registry.GroupMessage{}
	}
	return toolJSON(map[string]any{"ok": true, "mentions": out}), nil
}

// ─── helpers ─────────────────────────────────────────────────────────────

// stringSliceArg pulls a JSON array argument and returns its strings.
// LLM clients may emit either an array of strings or a single string;
// both are coerced. Returns nil for absent / empty / wrong-type.
func stringSliceArg(req mcp.CallToolRequest, key string) []string {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

// mapGroupErr converts a registry service error to an MCP tool-error
// envelope. *ErrAmbiguousMention surfaces with a structured
// `candidates` array so the agent can disambiguate without a second
// round-trip.
func mapGroupErr(err error) *mcp.CallToolResult {
	var amb *registry.ErrAmbiguousMention
	if errors.As(err, &amb) {
		// Custom envelope: invalid_request + candidates. Reuses the
		// toolError shape but the candidates list lives at the top
		// level of the JSON body so a calling agent can find it
		// without nesting.
		b, _ := json.Marshal(map[string]any{
			"ok":         false,
			"code":       "invalid_request",
			"message":    amb.Error(),
			"token":      amb.Token,
			"candidates": amb.Candidates,
		})
		return mcp.NewToolResultError(string(b))
	}
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return toolError("not_found", err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		return toolError("invalid_request", err.Error())
	case errors.Is(err, registry.ErrForbidden):
		return toolError("forbidden", err.Error())
	case errors.Is(err, registry.ErrGroupArchived):
		return toolError("locked", err.Error())
	case errors.Is(err, registry.ErrMintExhausted):
		return toolError("internal_error", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}
