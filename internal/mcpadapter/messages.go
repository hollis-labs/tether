package mcpadapter

import (
	"context"
	"fmt"

	gomcp "github.com/hollis-labs/go-mcp/server"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/client"
)

// validMsgKinds is the closed set of allowed kind values for mux_message_send.
// Wire names match go-messaging Kind constants; "response" is the wire name
// for replies (human docs say "reply" but the wire says "response").
var validMsgKinds = map[messaging.Kind]struct{}{
	messaging.MsgKindRequest:      {},
	messaging.MsgKindResponse:     {},
	messaging.MsgKindNotice:       {},
	messaging.MsgKindStatusUpdate: {},
	messaging.MsgKindHandoff:      {},
	messaging.MsgKindEscalation:   {},
}

func (a *Adapter) registerMessageTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_send",
		Description: "Send a message envelope via the agent-mux messaging store. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"from":         strProp("Sender URN (e.g. msg://agent/agent-mux/orchestrator)"),
			"to":           strProp("Recipient URN (e.g. msg://agent/agent-mux/worker)"),
			"kind":         strProp("Message kind: request, response, notice, status_update, handoff, escalation"),
			"payload_json": strProp("JSON payload body (optional)"),
			"thread_id":    strProp("Thread ID for grouping related messages (optional)"),
			"in_reply_to":  strProp("Message ID this message is in reply to (optional)"),
		}, "from", "to", "kind"),
		Handler: a.handleMessageSend,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_notify",
		Description: "Send a message envelope and best-effort wake a live recipient session with a mailbox notification turn. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"from":         strProp("Sender URN (e.g. msg://agent/agent-mux/orchestrator)"),
			"to":           strProp("Recipient URN; msg://session/<authority>/<session_id> wakes that session, msg://agent/<authority>/<logical_agent_id> wakes the latest running session for that logical agent when found"),
			"kind":         strProp("Message kind: request, response, notice, status_update, handoff, escalation (default notice)"),
			"payload_json": strProp("JSON payload body (optional)"),
			"thread_id":    strProp("Thread ID for grouping related messages (optional)"),
			"in_reply_to":  strProp("Message ID this message is in reply to (optional)"),
			"urgency":      strProp("Urgency: very-low, low, normal, high (default normal)"),
			"session_id":   strProp("Explicit live session ID to wake (optional override)"),
			"wake_text":    strProp("Override daemon-generated mailbox wake text (optional)"),
			"no_wake":      boolProp("Store the message but skip wake injection"),
		}, "from", "to"),
		Handler: a.handleMessageNotify,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_get",
		Description: "Get a message envelope by ID.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
			"as":         strProp("Sender or recipient URN claiming this read"),
		}, "message_id", "as"),
		Handler: a.handleMessageGet,
	}, Reads("GET /messages/{id}"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_inbox",
		Description: "Pull a recipient's undelivered messages (atomic-delivery agent pull model). DESTRUCTIVE: returned messages are marked delivered and will not appear in a future inbox call. For a non-destructive, repeatable listing use mux_message_list instead.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"to":        strProp("Recipient URN"),
			"kind":      strProp("Comma-separated kind filter: request, response, notice, status_update, handoff, escalation"),
			"thread_id": strProp("Thread ID filter (optional)"),
		}, "to"),
		Handler: a.handleMessageInbox,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_list",
		Description: "List a recipient's messages non-destructively. Repeatable: no delivered_at/read_at side effects. Each message carries read_at/archived_at state and a subject/body payload projection. Archived messages are excluded unless include_archived is set.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"to":               strProp("Recipient URN"),
			"kind":             strProp("Comma-separated kind filter: request, response, notice, status_update, handoff, escalation"),
			"thread_id":        strProp("Thread ID filter (optional)"),
			"include_archived": boolProp("Include archived messages (default false)"),
			"unread_only":      boolProp("Return only unread messages (default false)"),
			"limit":            numProp("Max results per page, clamped to [1,100] (default 100)"),
			"offset":           numProp("Number of messages to skip for pagination (default 0)"),
		}, "to"),
		Handler: a.handleMessageList,
	}, Reads("GET /messages/list: does NOT mark delivered, unlike inbox"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_thread",
		Description: "List all messages in a thread by thread ID, scoped to the ones involving the claimed identity.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"thread_id": strProp("Thread ID"),
			"as":        strProp("Sender or recipient URN claiming this read"),
			"kind":      strProp("Comma-separated kind filter (optional)"),
		}, "thread_id", "as"),
		Handler: a.handleMessageThread,
	}, Reads("GET /messages/thread/{id}"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_consume",
		Description: "Mark a message as consumed by the recipient. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
			"as":         strProp("Recipient URN consuming the message"),
		}, "message_id", "as"),
		Handler: a.handleMessageConsume,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_cancel",
		Description: "Cancel a pending message. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
		}, "message_id"),
		Handler: a.handleMessageCancel,
	}, Destroys("sets canceled_at one-way; the message can never be delivered and there is no uncancel"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_mark_read",
		Description: "Mark a message as read by its recipient (idempotent). Does not consume or delete it. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
			"as":         strProp("Recipient URN marking the message read"),
		}, "message_id", "as"),
		Handler: a.handleMessageMarkRead,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_archive",
		Description: "Archive (soft-delete) a message for its recipient (idempotent). Archived messages drop out of default mux_message_list results. Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
			"as":         strProp("Recipient URN archiving the message"),
		}, "message_id", "as"),
		Handler: a.handleMessageArchive,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_message_unarchive",
		Description: "Restore an archived message for its recipient (idempotent). Requires message.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"message_id": strProp("Message ID"),
			"as":         strProp("Recipient URN restoring the message"),
		}, "message_id", "as"),
		Handler: a.handleMessageUnarchive,
	}, Writes())
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleMessageSend(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	fromURN := str(args, "from")
	toURN := str(args, "to")
	kind := str(args, "kind")
	if fromURN == "" || toURN == "" || kind == "" {
		return nil, toolError("invalid_request", "from, to, and kind are required")
	}
	// Validate kind against closed enum before touching the store.
	msgKind := messaging.Kind(kind)
	if _, ok := validMsgKinds[msgKind]; !ok {
		return nil, toolError("invalid_request",
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", kind))
	}
	// Validate URN shape before a daemon round trip, matching the other
	// handlers' fail-fast behavior.
	if _, parseErr := messaging.ParseURN(fromURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid from URN: "+parseErr.Error())
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid to URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_send requires daemon routing; start MCP with mux mcp")
	}
	sendReq := client.MessageSendRequest{
		From:      fromURN,
		To:        toURN,
		Kind:      kind,
		ThreadID:  str(args, "thread_id"),
		InReplyTo: str(args, "in_reply_to"),
	}
	if p := str(args, "payload_json"); p != "" {
		sendReq.Payload = []byte(p)
	}
	sent, err := a.client.MessageSend(ctx, sendReq)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "message": sent}), nil
}

func (a *Adapter) handleMessageNotify(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	fromURN := str(args, "from")
	toURN := str(args, "to")
	if fromURN == "" || toURN == "" {
		return nil, toolError("invalid_request", "from and to are required")
	}
	kind := str(args, "kind")
	if kind == "" {
		kind = string(messaging.MsgKindNotice)
	}
	msgKind := messaging.Kind(kind)
	if _, ok := validMsgKinds[msgKind]; !ok {
		return nil, toolError("invalid_request",
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", kind))
	}
	if _, parseErr := messaging.ParseURN(fromURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid from URN: "+parseErr.Error())
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid to URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_notify requires daemon routing; start MCP with mux mcp")
	}
	wake := !boolArg(args, "no_wake")
	notifyReq := client.MessageNotifyRequest{
		MessageSendRequest: client.MessageSendRequest{
			From:      fromURN,
			To:        toURN,
			Kind:      kind,
			ThreadID:  str(args, "thread_id"),
			InReplyTo: str(args, "in_reply_to"),
		},
		Urgency:   str(args, "urgency"),
		SessionID: str(args, "session_id"),
		Wake:      &wake,
		WakeText:  str(args, "wake_text"),
	}
	if p := str(args, "payload_json"); p != "" {
		notifyReq.Payload = []byte(p)
		notifyReq.ContentType = "application/json"
	}
	out, err := a.client.MessageNotify(ctx, notifyReq)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "result": out}), nil
}

func (a *Adapter) handleMessageGet(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "message_id")
	asURN := str(args, "as")
	if id == "" || asURN == "" {
		return nil, toolError("invalid_request", "message_id and as are required")
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid as URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_get requires daemon routing; start MCP with mux mcp")
	}
	env, err := a.client.MessageGet(ctx, id, asURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "message": env}), nil
}

func (a *Adapter) handleMessageInbox(ctx context.Context, args map[string]any) (any, error) {
	toURN := str(args, "to")
	if toURN == "" {
		return nil, toolError("invalid_request", "to required")
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid to URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_inbox requires daemon routing; start MCP with mux mcp")
	}
	envs, err := a.client.MessageInbox(ctx, toURN, str(args, "kind"), str(args, "thread_id"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "messages": envs, "count": len(envs)}), nil
}

func (a *Adapter) handleMessageThread(ctx context.Context, args map[string]any) (any, error) {
	threadID := str(args, "thread_id")
	asURN := str(args, "as")
	if threadID == "" || asURN == "" {
		return nil, toolError("invalid_request", "thread_id and as are required")
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid as URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_thread requires daemon routing; start MCP with mux mcp")
	}
	envs, err := a.client.MessageThread(ctx, threadID, asURN, str(args, "kind"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{"ok": true, "messages": envs, "count": len(envs)}), nil
}

func (a *Adapter) handleMessageConsume(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	id := str(args, "message_id")
	asURN := str(args, "as")
	if id == "" || asURN == "" {
		return nil, toolError("invalid_request", "message_id and as are required")
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid as URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_consume requires daemon routing; start MCP with mux mcp")
	}
	if err := a.client.MessageConsume(ctx, id, asURN); err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

func (a *Adapter) handleMessageCancel(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	id := str(args, "message_id")
	if id == "" {
		return nil, toolError("invalid_request", "message_id required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_cancel requires daemon routing; start MCP with mux mcp")
	}
	if err := a.client.MessageCancel(ctx, id); err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

func (a *Adapter) handleMessageList(ctx context.Context, args map[string]any) (any, error) {
	toURN := str(args, "to")
	if toURN == "" {
		return nil, toolError("invalid_request", "to required")
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid to URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", "mux_message_list requires daemon routing; start MCP with mux mcp")
	}
	page, err := a.client.MessageList(ctx, toURN, str(args, "kind"), str(args, "thread_id"),
		boolArg(args, "include_archived"), boolArg(args, "unread_only"),
		intArg(args, "limit", 0), intArg(args, "offset", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"messages": page.Messages,
		"count":    len(page.Messages),
		"total":    page.Total,
		"limit":    page.Limit,
		"offset":   page.Offset,
	}), nil
}

func (a *Adapter) handleMessageMarkRead(ctx context.Context, args map[string]any) (any, error) {
	return a.messageRecipientAction(ctx, args, "mux_message_mark_read", a.client.MessageMarkRead)
}

func (a *Adapter) handleMessageArchive(ctx context.Context, args map[string]any) (any, error) {
	return a.messageRecipientAction(ctx, args, "mux_message_archive", a.client.MessageArchive)
}

func (a *Adapter) handleMessageUnarchive(ctx context.Context, args map[string]any) (any, error) {
	return a.messageRecipientAction(ctx, args, "mux_message_unarchive", a.client.MessageUnarchive)
}

// messageRecipientAction is the shared body for the recipient-scoped,
// idempotent state transitions (mark_read / archive / unarchive), now
// routed through the daemon HTTP client rather than the in-process store
// (T05: closes the mux-mcp-subprocess bypass, see internal/store's
// T01/T03 findings on the separate-SQLite-connection gap). Each requires
// message.write scope plus message_id + as arguments.
func (a *Adapter) messageRecipientAction(ctx context.Context, args map[string]any, toolName string,
	fn func(context.Context, string, string) error) (any, error) {
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		return nil, err
	}
	id := str(args, "message_id")
	asURN := str(args, "as")
	if id == "" || asURN == "" {
		return nil, toolError("invalid_request", "message_id and as are required")
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return nil, toolError("invalid_request", "invalid as URN: "+parseErr.Error())
	}
	if a.client == nil {
		return nil, toolError("internal_error", toolName+" requires daemon routing; start MCP with mux mcp")
	}
	if err := fn(ctx, id, asURN); err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, id)
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

// boolArg extracts a boolean argument. JSON booleans arrive as bool;
// LLM clients sometimes emit "true"/"1" strings. Both forms are handled;
// any other value (or absence) yields false.
func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}
