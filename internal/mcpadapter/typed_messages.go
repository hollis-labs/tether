package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

// Typed message and seat status tools (CW-20261114-0013, design v2 sections 5
// and 8). The daemon validates the envelope and resolves every @handle; a bad
// call comes back as invalid_request naming the field and the reason, so fix it
// and call again.

var envelopeFields = []string{"kind", "to", "re", "in_reply_to", "attention", "awaiting", "intent", "subject", "body", "options", "default_option", "answer"}

func (a *Adapter) registerTypedMessageTools(s *gomcp.Server) {
	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_message_post",
		Description: "Send a typed message to seats or Chrispian by @handle: the way to ask, answer, hand off, escalate or request approval. " +
			"The server stamps you as the sender, checks every @handle and the envelope, and refuses a bad one with the field and the reason. " +
			"question and decision need 2-6 options (mark one recommended); approval_request without options is approve/deny. " +
			"Answer a question by replying with in_reply_to and answer.option. Requires message.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringEnumProp("kind", "What this is; Tachyon renders each as a card", true, "question", "update", "handoff", "escalation", "approval_request", "decision", "message"),
			gomcp.StringArrayProp("to", "Recipients as @handles, e.g. [\"@archie\"]; @chrispian is the human operator", true),
			gomcp.StringProp("body", "Markdown, at most 4000 characters; short, the receiver asks for more", true),
			gomcp.StringProp("subject", "One-line card title, at most 120 characters (optional; default the body's first line)", false),
			gomcp.StringProp("re", "What it is about: task:<task id>, thread:<id> or topic:<slug> (optional)", false),
			gomcp.StringProp("in_reply_to", "Message id this answers (optional); joins its thread", false),
			gomcp.StringEnumProp("attention", "Who must look: human, agent or none (default human when @chrispian is a recipient or awaited, else agent)", false, "human", "agent", "none"),
			gomcp.StringProp("awaiting", "@handle that must act next (optional)", false),
			gomcp.StringProp("intent", "Short routing tag, [a-z0-9._-]{1,40} (optional)", false),
			gomcp.ArrayProp("options", "question/decision/approval_request choices: [{id, label, recommended?, detail?}]", false, map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string"}, "label": map[string]interface{}{"type": "string"},
					"recommended": map[string]interface{}{"type": "boolean"}, "detail": map[string]interface{}{"type": "string"},
				},
				"required": []string{"id", "label"},
			}),
			gomcp.StringProp("default_option", "Option id that applies if nobody answers (optional)", false),
			gomcp.ObjectProp("answer", "On a reply to a question/decision/approval_request: {option, note?}", false),
			gomcp.StringEnumProp("urgency", "Delivery urgency (default normal); a critical approval is an approval_request at high", false, "very-low", "low", "normal", "high"),
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
			gomcp.StringProp("idempotency_key", "Optional key; repeating it returns the earlier send instead of sending again", false),
		),
		Handler: a.handleMessagePost,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_pending_replies",
		Description: "List typed messages waiting on you: unread questions, decisions and approval requests addressed to you, and anything awaiting your @handle, " +
			"that you have not replied to. Reply with tether_message_post (in_reply_to) or mark the message read.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
		),
		Handler: a.handlePendingReplies,
	}), Reads("lists pending typed messages; changes nothing"))

	a.addTool(s, strictArgs(gomcp.Tool{
		Name:        "tether_stop_gate_report",
		Description: "Used by the Stop-hook reply gate: record that it blocked this seat's turn on these pending typed messages. Publishes seat.gate_blocked; changes no message. Requires message.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringArrayProp("message_ids", "The pending message ids the gate blocked on (1 to 20)", true),
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
		),
		Handler: a.handleStopGateReport,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_status_set",
		Description: "Declare your seat's status: available, working, waiting-decision, waiting-agent, blocked, paused or offline, with a short reason, a ref and an optional expiry. " +
			"blocked must name an owner (@handle). Re-setting the same status only refreshes its expiry. The operator or a manager may set another seat's status with target. " +
			"Cheap and never a gate: call it after the work it describes, not instead of it.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringEnumProp("state", "Declared state", true, "available", "working", "waiting-decision", "waiting-agent", "blocked", "paused", "offline"),
			gomcp.StringProp("reason", "One line, at most 200 characters (optional)", false),
			gomcp.StringProp("ref", "task:<task id> or an https:// URL (optional)", false),
			gomcp.StringProp("owner", "@handle that owns the blocker; required for blocked", false),
			gomcp.IntegerProp("ttl_seconds", "Expire after this many seconds (optional; at most 7 days)", false),
			gomcp.StringProp("expires_at", "Expire at this RFC 3339 time (optional; alternative to ttl_seconds)", false),
			gomcp.StringProp("target", "@handle of another seat (operator or manager only; default yourself)", false),
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
		),
		Handler: a.handleStatusSet,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_handle_set",
		Description: "Give an existing registry agent a @handle so typed messages can address it (for seats that are not catalog agents; catalog agents get theirs from their id or handle: key). " +
			"Operator only; a handle another agent holds is refused. Requires registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("urn", "The agent's registry URN (msg://agent/<authority>/<id>)", true),
			gomcp.StringProp("handle", "@name: lowercase letters, digits, '-' or '_', starting with a letter", true),
		),
		Handler: a.handleHandleSet,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name:        "tether_status_get",
		Description: "A seat's status card: what it declared (state, reason, ref, owner, since, expiry) beside what the hub observes (session, online, health, work state, last activity, unread mail, pending replies).",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("handle", "The seat's @handle", true),
		),
		Handler: a.handleStatusGet,
	}), Reads("reads a status card; changes nothing"))
}

func (a *Adapter) daemonClientFor(tool string) error {
	if a.client == nil {
		return toolError("internal_error", tool+" requires daemon routing; start MCP with tether mcp")
	}
	return nil
}

func (a *Adapter) handleMessagePost(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_message_post"); err != nil {
		return nil, err
	}
	env := map[string]any{}
	for _, k := range envelopeFields {
		if v, ok := args[k]; ok && v != nil && v != "" {
			env[k] = v
		}
	}
	// A single recipient given as a string is accepted as a one-item list.
	if to, ok := env["to"].(string); ok {
		env["to"] = []string{to}
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, toolError("invalid_request", "envelope: "+err.Error())
	}
	out, err := a.client.MessagePost(ctx, str(args, "as"), str(args, "urgency"), raw, str(args, "idempotency_key"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "ids": out.IDs, "thread_id": out.ThreadID, "envelope": out.Envelope}), nil
}

func (a *Adapter) handlePendingReplies(ctx context.Context, args map[string]any) (any, error) {
	if err := a.daemonClientFor("tether_pending_replies"); err != nil {
		return nil, err
	}
	out, err := a.client.PendingReplies(ctx, str(args, "as"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleStatusSet(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_status_set"); err != nil {
		return nil, err
	}
	body := map[string]any{}
	for _, k := range []string{"state", "reason", "ref", "owner", "expires_at", "target"} {
		if v := strings.TrimSpace(str(args, k)); v != "" {
			body[k] = v
		}
	}
	if v, ok := args["ttl_seconds"].(float64); ok && v > 0 {
		body["ttl_seconds"] = int(v)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, toolError("invalid_request", err.Error())
	}
	out, err := a.client.StatusSet(ctx, str(args, "as"), raw)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleStatusGet(ctx context.Context, args map[string]any) (any, error) {
	if err := a.daemonClientFor("tether_status_get"); err != nil {
		return nil, err
	}
	h := strings.TrimSpace(str(args, "handle"))
	if h == "" {
		return nil, toolError("invalid_request", "handle is required")
	}
	out, err := a.client.StatusGet(ctx, h)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleHandleSet(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_handle_set"); err != nil {
		return nil, err
	}
	out, err := a.client.HandleSet(ctx, str(args, "urn"), str(args, "handle"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleStopGateReport(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_stop_gate_report"); err != nil {
		return nil, err
	}
	var ids []string
	if raw, ok := args["message_ids"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	out, err := a.client.StopGateReport(ctx, str(args, "as"), ids)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}
