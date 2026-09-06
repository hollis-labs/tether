package mcpadapter

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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

func (a *Adapter) registerMessageTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_message_send",
		mcp.WithDescription("Send a message envelope via the agent-mux messaging store. Requires message.write scope."),
		mcp.WithString("from", mcp.Required(), mcp.Description("Sender URN (e.g. msg://agent/agent-mux/orchestrator)")),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN (e.g. msg://agent/agent-mux/worker)")),
		mcp.WithString("kind", mcp.Required(), mcp.Description("Message kind: request, response, notice, status_update, handoff, escalation")),
		mcp.WithString("payload_json", mcp.Description("JSON payload body (optional)")),
		mcp.WithString("thread_id", mcp.Description("Thread ID for grouping related messages (optional)")),
		mcp.WithString("in_reply_to", mcp.Description("Message ID this message is in reply to (optional)")),
	), a.handleMessageSend)

	a.addTool(s, mcp.NewTool("mux_message_notify",
		mcp.WithDescription("Send a message envelope and best-effort wake a live recipient session with a mailbox notification turn. Requires message.write scope."),
		mcp.WithString("from", mcp.Required(), mcp.Description("Sender URN (e.g. msg://agent/agent-mux/orchestrator)")),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN; msg://session/<authority>/<session_id> wakes that session, msg://agent/<authority>/<logical_agent_id> wakes the latest running session for that logical agent when found")),
		mcp.WithString("kind", mcp.Description("Message kind: request, response, notice, status_update, handoff, escalation (default notice)")),
		mcp.WithString("payload_json", mcp.Description("JSON payload body (optional)")),
		mcp.WithString("thread_id", mcp.Description("Thread ID for grouping related messages (optional)")),
		mcp.WithString("in_reply_to", mcp.Description("Message ID this message is in reply to (optional)")),
		mcp.WithString("urgency", mcp.Description("Urgency: very-low, low, normal, high (default normal)")),
		mcp.WithString("session_id", mcp.Description("Explicit live session ID to wake (optional override)")),
		mcp.WithString("wake_text", mcp.Description("Override daemon-generated mailbox wake text (optional)")),
		mcp.WithBoolean("no_wake", mcp.Description("Store the message but skip wake injection")),
	), a.handleMessageNotify)

	a.addTool(s, mcp.NewTool("mux_message_get",
		mcp.WithDescription("Get a message envelope by ID."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Sender or recipient URN claiming this read")),
	), a.handleMessageGet)

	a.addTool(s, mcp.NewTool("mux_message_inbox",
		mcp.WithDescription("Pull a recipient's undelivered messages (atomic-delivery agent pull model). DESTRUCTIVE: returned messages are marked delivered and will not appear in a future inbox call. For a non-destructive, repeatable listing use mux_message_list instead."),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN")),
		mcp.WithString("kind", mcp.Description("Comma-separated kind filter: request, response, notice, status_update, handoff, escalation")),
		mcp.WithString("thread_id", mcp.Description("Thread ID filter (optional)")),
	), a.handleMessageInbox)

	a.addTool(s, mcp.NewTool("mux_message_list",
		mcp.WithDescription("List a recipient's messages non-destructively. Repeatable: no delivered_at/read_at side effects. Each message carries read_at/archived_at state and a subject/body payload projection. Archived messages are excluded unless include_archived is set."),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN")),
		mcp.WithString("kind", mcp.Description("Comma-separated kind filter: request, response, notice, status_update, handoff, escalation")),
		mcp.WithString("thread_id", mcp.Description("Thread ID filter (optional)")),
		mcp.WithBoolean("include_archived", mcp.Description("Include archived messages (default false)")),
		mcp.WithBoolean("unread_only", mcp.Description("Return only unread messages (default false)")),
		mcp.WithNumber("limit", mcp.Description("Max results per page, clamped to [1,100] (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Number of messages to skip for pagination (default 0)")),
	), a.handleMessageList)

	a.addTool(s, mcp.NewTool("mux_message_thread",
		mcp.WithDescription("List all messages in a thread by thread ID, scoped to the ones involving the claimed identity."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("Thread ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Sender or recipient URN claiming this read")),
		mcp.WithString("kind", mcp.Description("Comma-separated kind filter (optional)")),
	), a.handleMessageThread)

	a.addTool(s, mcp.NewTool("mux_message_consume",
		mcp.WithDescription("Mark a message as consumed by the recipient. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Recipient URN consuming the message")),
	), a.handleMessageConsume)

	a.addTool(s, mcp.NewTool("mux_message_cancel",
		mcp.WithDescription("Cancel a pending message. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
	), a.handleMessageCancel)

	a.addTool(s, mcp.NewTool("mux_message_mark_read",
		mcp.WithDescription("Mark a message as read by its recipient (idempotent). Does not consume or delete it. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Recipient URN marking the message read")),
	), a.handleMessageMarkRead)

	a.addTool(s, mcp.NewTool("mux_message_archive",
		mcp.WithDescription("Archive (soft-delete) a message for its recipient (idempotent). Archived messages drop out of default mux_message_list results. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Recipient URN archiving the message")),
	), a.handleMessageArchive)

	a.addTool(s, mcp.NewTool("mux_message_unarchive",
		mcp.WithDescription("Restore an archived message for its recipient (idempotent). Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Recipient URN restoring the message")),
	), a.handleMessageUnarchive)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleMessageSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeMessageWrite); denied != nil {
		return denied, nil
	}
	fromURN := str(req, "from")
	toURN := str(req, "to")
	kind := str(req, "kind")
	if fromURN == "" || toURN == "" || kind == "" {
		return toolError("invalid_request", "from, to, and kind are required"), nil
	}
	// Validate kind against closed enum before touching the store.
	msgKind := messaging.Kind(kind)
	if _, ok := validMsgKinds[msgKind]; !ok {
		return toolError("invalid_request",
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", kind)), nil
	}
	// Validate URN shape before a daemon round trip, matching the other
	// handlers' fail-fast behavior.
	if _, parseErr := messaging.ParseURN(fromURN); parseErr != nil {
		return toolError("invalid_request", "invalid from URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_send requires daemon routing; start MCP with mux mcp"), nil
	}
	sendReq := client.MessageSendRequest{
		From:      fromURN,
		To:        toURN,
		Kind:      kind,
		ThreadID:  str(req, "thread_id"),
		InReplyTo: str(req, "in_reply_to"),
	}
	if p := str(req, "payload_json"); p != "" {
		sendReq.Payload = []byte(p)
	}
	sent, err := a.client.MessageSend(ctx, sendReq)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, ""), nil
	}
	return toolJSON(map[string]any{"ok": true, "message": sent}), nil
}

func (a *Adapter) handleMessageNotify(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeMessageWrite); denied != nil {
		return denied, nil
	}
	fromURN := str(req, "from")
	toURN := str(req, "to")
	if fromURN == "" || toURN == "" {
		return toolError("invalid_request", "from and to are required"), nil
	}
	kind := str(req, "kind")
	if kind == "" {
		kind = string(messaging.MsgKindNotice)
	}
	msgKind := messaging.Kind(kind)
	if _, ok := validMsgKinds[msgKind]; !ok {
		return toolError("invalid_request",
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", kind)), nil
	}
	if _, parseErr := messaging.ParseURN(fromURN); parseErr != nil {
		return toolError("invalid_request", "invalid from URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_notify requires daemon routing; start MCP with mux mcp"), nil
	}
	wake := !boolArg(req, "no_wake")
	notifyReq := client.MessageNotifyRequest{
		MessageSendRequest: client.MessageSendRequest{
			From:      fromURN,
			To:        toURN,
			Kind:      kind,
			ThreadID:  str(req, "thread_id"),
			InReplyTo: str(req, "in_reply_to"),
		},
		Urgency:   str(req, "urgency"),
		SessionID: str(req, "session_id"),
		Wake:      &wake,
		WakeText:  str(req, "wake_text"),
	}
	if p := str(req, "payload_json"); p != "" {
		notifyReq.Payload = []byte(p)
		notifyReq.ContentType = "application/json"
	}
	out, err := a.client.MessageNotify(ctx, notifyReq)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, ""), nil
	}
	return toolJSON(map[string]any{"ok": true, "result": out}), nil
}

func (a *Adapter) handleMessageGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "message_id")
	asURN := str(req, "as")
	if id == "" || asURN == "" {
		return toolError("invalid_request", "message_id and as are required"), nil
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return toolError("invalid_request", "invalid as URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_get requires daemon routing; start MCP with mux mcp"), nil
	}
	env, err := a.client.MessageGet(ctx, id, asURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, id), nil
	}
	return toolJSON(map[string]any{"ok": true, "message": env}), nil
}

func (a *Adapter) handleMessageInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	toURN := str(req, "to")
	if toURN == "" {
		return toolError("invalid_request", "to required"), nil
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_inbox requires daemon routing; start MCP with mux mcp"), nil
	}
	envs, err := a.client.MessageInbox(ctx, toURN, str(req, "kind"), str(req, "thread_id"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, ""), nil
	}
	return toolJSON(map[string]any{"ok": true, "messages": envs, "count": len(envs)}), nil
}

func (a *Adapter) handleMessageThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	threadID := str(req, "thread_id")
	asURN := str(req, "as")
	if threadID == "" || asURN == "" {
		return toolError("invalid_request", "thread_id and as are required"), nil
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return toolError("invalid_request", "invalid as URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_thread requires daemon routing; start MCP with mux mcp"), nil
	}
	envs, err := a.client.MessageThread(ctx, threadID, asURN, str(req, "kind"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, ""), nil
	}
	return toolJSON(map[string]any{"ok": true, "messages": envs, "count": len(envs)}), nil
}

func (a *Adapter) handleMessageConsume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeMessageWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "message_id")
	asURN := str(req, "as")
	if id == "" || asURN == "" {
		return toolError("invalid_request", "message_id and as are required"), nil
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return toolError("invalid_request", "invalid as URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_consume requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.MessageConsume(ctx, id, asURN); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, id), nil
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

func (a *Adapter) handleMessageCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeMessageWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "message_id")
	if id == "" {
		return toolError("invalid_request", "message_id required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_cancel requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.MessageCancel(ctx, id); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, id), nil
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

func (a *Adapter) handleMessageList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	toURN := str(req, "to")
	if toURN == "" {
		return toolError("invalid_request", "to required"), nil
	}
	if _, parseErr := messaging.ParseURN(toURN); parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", "mux_message_list requires daemon routing; start MCP with mux mcp"), nil
	}
	page, err := a.client.MessageList(ctx, toURN, str(req, "kind"), str(req, "thread_id"),
		boolArg(req, "include_archived"), boolArg(req, "unread_only"),
		intArg(req, "limit", 0), intArg(req, "offset", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, ""), nil
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

func (a *Adapter) handleMessageMarkRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return a.messageRecipientAction(ctx, req, "mux_message_mark_read", a.client.MessageMarkRead)
}

func (a *Adapter) handleMessageArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return a.messageRecipientAction(ctx, req, "mux_message_archive", a.client.MessageArchive)
}

func (a *Adapter) handleMessageUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return a.messageRecipientAction(ctx, req, "mux_message_unarchive", a.client.MessageUnarchive)
}

// messageRecipientAction is the shared body for the recipient-scoped,
// idempotent state transitions (mark_read / archive / unarchive), now
// routed through the daemon HTTP client rather than the in-process store
// (T05: closes the mux-mcp-subprocess bypass, see internal/store's
// T01/T03 findings on the separate-SQLite-connection gap). Each requires
// message.write scope plus message_id + as arguments.
func (a *Adapter) messageRecipientAction(ctx context.Context, req mcp.CallToolRequest, toolName string,
	fn func(context.Context, string, string) error) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeMessageWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "message_id")
	asURN := str(req, "as")
	if id == "" || asURN == "" {
		return toolError("invalid_request", "message_id and as are required"), nil
	}
	if _, parseErr := messaging.ParseURN(asURN); parseErr != nil {
		return toolError("invalid_request", "invalid as URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if a.client == nil {
		return toolError("internal_error", toolName+" requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := fn(ctx, id, asURN); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return classifyClientErr(err, id), nil
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}

// boolArg extracts a boolean argument. JSON booleans arrive as bool;
// LLM clients sometimes emit "true"/"1" strings. Both forms are handled;
// any other value (or absence) yields false.
func boolArg(req mcp.CallToolRequest, key string) bool {
	switch v := req.GetArguments()[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}
