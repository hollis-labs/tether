package mcpadapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	messaging "github.com/hollis-labs/go-messaging"
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
	s.AddTool(mcp.NewTool("mux_message_send",
		mcp.WithDescription("Send a message envelope via the agent-mux messaging store. Requires message.write scope."),
		mcp.WithString("from", mcp.Required(), mcp.Description("Sender URN (e.g. msg://agent/agent-mux/orchestrator)")),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN (e.g. msg://agent/agent-mux/worker)")),
		mcp.WithString("kind", mcp.Required(), mcp.Description("Message kind: request, response, notice, status_update, handoff, escalation")),
		mcp.WithString("payload_json", mcp.Description("JSON payload body (optional)")),
		mcp.WithString("thread_id", mcp.Description("Thread ID for grouping related messages (optional)")),
		mcp.WithString("in_reply_to", mcp.Description("Message ID this message is in reply to (optional)")),
	), a.handleMessageSend)

	s.AddTool(mcp.NewTool("mux_message_get",
		mcp.WithDescription("Get a message envelope by ID."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
	), a.handleMessageGet)

	s.AddTool(mcp.NewTool("mux_message_inbox",
		mcp.WithDescription("List messages in a recipient's inbox. Optionally filter by kind and/or thread."),
		mcp.WithString("to", mcp.Required(), mcp.Description("Recipient URN")),
		mcp.WithString("kind", mcp.Description("Comma-separated kind filter: request, response, notice, status_update, handoff, escalation")),
		mcp.WithString("thread_id", mcp.Description("Thread ID filter (optional)")),
	), a.handleMessageInbox)

	s.AddTool(mcp.NewTool("mux_message_thread",
		mcp.WithDescription("List all messages in a thread by thread ID."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("Thread ID")),
		mcp.WithString("kind", mcp.Description("Comma-separated kind filter (optional)")),
	), a.handleMessageThread)

	s.AddTool(mcp.NewTool("mux_message_consume",
		mcp.WithDescription("Mark a message as consumed by the recipient. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
		mcp.WithString("as", mcp.Required(), mcp.Description("Recipient URN consuming the message")),
	), a.handleMessageConsume)

	s.AddTool(mcp.NewTool("mux_message_cancel",
		mcp.WithDescription("Cancel a pending message. Requires message.write scope."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
	), a.handleMessageCancel)
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
	from, parseErr := messaging.ParseURN(fromURN)
	if parseErr != nil {
		return toolError("invalid_request", "invalid from URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	to, parseErr := messaging.ParseURN(toURN)
	if parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	env := messaging.Envelope{
		From:      from,
		To:        to,
		Kind:      msgKind,
		ThreadID:  str(req, "thread_id"),
		InReplyTo: str(req, "in_reply_to"),
	}
	if p := str(req, "payload_json"); p != "" {
		env.Payload = []byte(p)
	}
	sent, err := a.svc.Store.MessagingStore().Send(ctx, env)
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "message": sent}), nil
}

func (a *Adapter) handleMessageGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "message_id")
	if id == "" {
		return toolError("invalid_request", "message_id required"), nil
	}
	env, err := a.svc.Store.MessagingStore().Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return toolError("not_found", "message not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "message": env}), nil
}

func (a *Adapter) handleMessageInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	toURN := str(req, "to")
	if toURN == "" {
		return toolError("invalid_request", "to required"), nil
	}
	to, parseErr := messaging.ParseURN(toURN)
	if parseErr != nil {
		return toolError("invalid_request", "invalid to URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	var f messaging.Filter
	if ks := str(req, "kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = str(req, "thread_id")
	envs, err := a.svc.Store.MessagingStore().Inbox(ctx, to, f)
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "messages": envs, "count": len(envs)}), nil
}

func (a *Adapter) handleMessageThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	threadID := str(req, "thread_id")
	if threadID == "" {
		return toolError("invalid_request", "thread_id required"), nil
	}
	var f messaging.Filter
	if ks := str(req, "kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	envs, err := a.svc.Store.MessagingStore().Thread(ctx, threadID, f)
	if err != nil {
		return toolError("internal_error", err.Error()), nil
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
	recipient, parseErr := messaging.ParseURN(asURN)
	if parseErr != nil {
		return toolError("invalid_request", "invalid as URN: "+parseErr.Error()), nil //nolint:nilerr
	}
	if err := a.svc.Store.MessagingStore().Consume(ctx, id, recipient); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "message not found: "+id), nil
		}
		if isWrongRecipient(err) {
			return toolError("conflict", "caller is not the intended recipient of message: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
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
	if err := a.svc.Store.MessagingStore().Cancel(ctx, id); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "message not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "message_id": id}), nil
}
