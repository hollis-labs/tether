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

var envelopeFields = []string{"kind", "to", "re", "in_reply_to", "attention", "awaiting", "intent", "subject", "body", "options", "default_option", "answer", "on_behalf_of", "drafted_by", "deliverable", "status", "evidence", "task_id"}

func (a *Adapter) registerTypedMessageTools(s *gomcp.Server) {
	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_message_post",
		Description: "Send a typed message to seats or Chrispian by @handle: the way to ask, answer, hand off, escalate or request approval. " +
			"The server stamps you as the sender, checks every @handle and the envelope, and refuses a bad one with the field and the reason. " +
			"question and decision need 2-6 options (mark one recommended); approval_request without options is approve/deny. " +
			"Answer a question by replying with in_reply_to and answer.option. Requires message.write scope.",
		InputSchema: messagePostInputSchema(),
		Handler:     a.handleMessagePost,
	}), Writes())

	optionsProp := gomcp.ArrayProp("options", "question/decision/approval_request choices: [{id, label, recommended?, detail?}]", false, map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string"}, "label": map[string]interface{}{"type": "string"},
			"recommended": map[string]interface{}{"type": "boolean"}, "detail": map[string]interface{}{"type": "string"},
		},
		"required": []string{"id", "label"},
	})
	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_ask",
		Description: "Ask a seat or Chrispian for something that needs an answer or an action: kind request (\"do X and report back\"; give a deliverable saying what done looks like), " +
			"question or decision (2-6 options), approval_request, handoff or escalation. Use it whenever someone must act or answer before you can go on. " +
			"On a Torque-connected hub, kind=request creates or links a durable Torque task and returns task_id; use one recipient and a stable idempotency_key. " +
			"The recipient is woken; its tether_reply comes back to you and wakes you. You are the sender (your session identity). " +
			"Do not use it for status or progress (use Torque checkpoints or tether_status_set) or for plain FYIs (tether_message_post kind update). Requires message.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringEnumProp("kind", "request: do X and report back; question/decision: choose among options; approval_request: approve or deny; handoff; escalation", true, "request", "question", "decision", "approval_request", "handoff", "escalation"),
			gomcp.StringArrayProp("to", "Recipients as @handles, e.g. [\"@archie\"]; @chrispian is the human operator", true),
			gomcp.StringProp("subject", "One-line title, at most 120 characters", true),
			gomcp.StringProp("body", "Markdown, at most 4000 characters: what you need and the context", true),
			gomcp.StringProp("task_id", "Existing Torque task to link as this request (optional; otherwise created)", false),
			gomcp.StringProp("deliverable", "For a request: what done looks like (PR merged, numbers, a file path...), at most 1000 characters", false),
			gomcp.StringProp("re", "What it is about: task:<task id>, thread:<id> or topic:<slug> (optional)", false),
			optionsProp,
			gomcp.StringProp("default_option", "Option id that applies if nobody answers (optional)", false),
			gomcp.StringEnumProp("attention", "Who must look: human, agent or none (default human when @chrispian is a recipient, else agent)", false, "human", "agent", "none"),
			gomcp.StringEnumProp("urgency", "Delivery urgency (default normal)", false, "very-low", "low", "normal", "high"),
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
			gomcp.StringProp("idempotency_key", "Required for tracked requests/final replies; repeating unchanged input returns the earlier send", false),
		),
		Handler: a.handleAsk,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_reply",
		Description: "Answer an ask (a message from tether_ask, or any mail that asked you something): the reply goes to the asker, wakes them, and carries in_reply_to. " +
			"Set status done, declined or blocked when it settles a request, list evidence (PR, commit, path, URL), and for a question/decision/approval_request give answer (an option id; it must be one of the ask's options). " +
			"Do not use it for status or progress updates (use Torque checkpoints) or to start a new conversation (tether_ask). Requires message.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("in_reply_to", "Id of the message you are answering", true),
			gomcp.StringProp("body", "Markdown, at most 4000 characters", true),
			gomcp.StringProp("answer", "Option id you choose, for a question, decision or approval_request (approve/deny when it had no options)", false),
			gomcp.StringProp("note", "One note on the answer, at most 1000 characters (optional)", false),
			gomcp.StringEnumProp("status", "How this settles a request: done, declined or blocked (optional)", false, "done", "declined", "blocked"),
			gomcp.StringArrayProp("evidence", "Pointers backing the reply: PR URL, commit, file path, event id (at most 10)", false),
			gomcp.StringProp("subject", "One-line title (optional)", false),
			gomcp.StringArrayProp("to", "Recipients as @handles (optional; default the asker)", false),
			gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
			gomcp.StringProp("idempotency_key", "Required for tracked requests/final replies; repeating unchanged input returns the earlier send", false),
		),
		Handler: a.handleReply,
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

// handleAsk builds and posts a typed ask (CW-20261117-0003). The daemon
// validates it and refuses a bad one with the field and the reason.
func (a *Adapter) handleAsk(ctx context.Context, args map[string]any) (any, error) {
	env := map[string]any{}
	for _, k := range []string{"kind", "to", "re", "subject", "body", "deliverable", "options", "default_option", "attention", "task_id"} {
		if v, ok := args[k]; ok && v != nil && v != "" {
			env[k] = v
		}
	}
	return a.postEnvelope(ctx, "tether_ask", env, args)
}

// handleReply builds and posts a typed reply: kind message, in_reply_to, and
// the answer, status and evidence. The daemon addresses it to the asker and
// awaits them, and checks answer against the ask's options.
func (a *Adapter) handleReply(ctx context.Context, args map[string]any) (any, error) {
	env := map[string]any{"kind": "message"}
	for _, k := range []string{"in_reply_to", "body", "status", "evidence", "subject", "to"} {
		if v, ok := args[k]; ok && v != nil && v != "" {
			env[k] = v
		}
	}
	if opt := strings.TrimSpace(str(args, "answer")); opt != "" {
		answer := map[string]any{"option": opt}
		if note := strings.TrimSpace(str(args, "note")); note != "" {
			answer["note"] = note
		}
		env["answer"] = answer
	} else if str(args, "note") != "" {
		return nil, toolError("invalid_request", "note: only with answer")
	}
	return a.postEnvelope(ctx, "tether_reply", env, args)
}

func (a *Adapter) postEnvelope(ctx context.Context, tool string, env map[string]any, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor(tool); err != nil {
		return nil, err
	}
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

// messagePostInputSchema declares every envelope field tether_message_post
// forwards (envelopeFields) plus as, urgency and idempotency_key. strictArgs
// refuses anything undeclared, so a field missing here cannot be sent at all
// (CW-20261118-0024: kind request needs deliverable).
func messagePostInputSchema() any {
	return gomcp.InputSchema(
		gomcp.StringEnumProp("kind", "What this is; Tachyon renders each as a card", true, "question", "update", "handoff", "escalation", "approval_request", "decision", "message", "request"),
		gomcp.StringArrayProp("to", "Recipients as @handles, e.g. [\"@archie\"]; @chrispian is the human operator", true),
		gomcp.StringProp("body", "Markdown, at most 4000 characters; short, the receiver asks for more", true),
		gomcp.StringProp("subject", "One-line card title, at most 120 characters (optional; default the body's first line)", false),
		gomcp.StringProp("re", "What it is about: task:<task id>, thread:<id> or topic:<slug> (optional)", false),
		gomcp.StringProp("in_reply_to", "Message id this answers (optional); joins its thread", false),
		gomcp.StringEnumProp("attention", "Who must look: human, agent or none (default human when @chrispian is a recipient or awaited, else agent)", false, "human", "agent", "none"),
		gomcp.StringProp("awaiting", "@handle that must act next (optional)", false),
		gomcp.StringProp("intent", "Short routing tag, [a-z0-9._-]{1,40} (optional)", false),
		gomcp.StringProp("on_behalf_of", "Who you are sending this for, as a @handle or URN, when you act for someone (e.g. an assistant for @chrispian). It is your claim, shown to recipients; it never makes the mail theirs (optional)", false),
		gomcp.StringEnumProp("drafted_by", "Who wrote it, with on_behalf_of only (default assistant)", false, "assistant", "human"),
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
		gomcp.StringProp("task_id", "Existing Torque task to link as this request (optional; otherwise created)", false),
		gomcp.StringProp("deliverable", "Required for kind request: what done looks like (PR merged, numbers, a file path...), at most 1000 characters; only a request carries one", false),
		gomcp.StringEnumProp("status", "On a reply (in_reply_to): how it settles a request: done, declined or blocked (optional)", false, "done", "declined", "blocked"),
		gomcp.StringArrayProp("evidence", "Pointers backing a reply: PR URL, commit, file path, event id (at most 10, one line each)", false),
		gomcp.StringEnumProp("urgency", "Delivery urgency (default normal); a critical approval is an approval_request at high", false, "very-low", "low", "normal", "high"),
		gomcp.StringProp("as", "Your address (optional; default your authenticated identity)", false),
		gomcp.StringProp("idempotency_key", "Required for tracked requests/final replies; repeating unchanged input returns the earlier send", false),
	)
}
