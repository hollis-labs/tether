package acpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// handleInitialize handles the inbound `initialize` request. Negotiates
// protocol version + capabilities and returns the agent identity. Per
// ACP spec, the client sends its preferred protocol version; the agent
// echoes it back if supported, or responds with its own latest if not.
func (a *Adapter) handleInitialize(_ context.Context, params json.RawMessage) (any, error) {
	var p InitializeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "initialize params: " + err.Error()}
		}
	}
	// Version negotiation: clamp to ProtocolVersion if client requests
	// something newer (we don't support it). If client is older we
	// downgrade silently — they'll see our version in the response.
	negotiated := p.ProtocolVersion
	if negotiated < 1 || negotiated > ProtocolVersion {
		negotiated = ProtocolVersion
	}
	// We accept whatever clientCapabilities the editor advertised, but
	// MVP code paths don't call back into the editor (no fs/* or
	// terminal/* origination), so we don't need to gate on them.

	return InitializeResult{
		ProtocolVersion: negotiated,
		AgentCapabilities: AgentCapabilities{
			LoadSession:        false, // see §1 lock — we support resume, not load
			PromptCapabilities: PromptCapabilities{Image: false, Audio: false, EmbeddedContext: false},
			McpCapabilities:    McpCapabilities{HTTP: false, SSE: false},
			SessionCapabilities: AgentSessionCapabilities{
				List:   false,
				Close:  true,
				Resume: true,
			},
		},
		AgentInfo: Implementation{
			Name:    a.agentName,
			Title:   "Agent Mux",
			Version: version,
		},
		AuthMethods: a.auth.AuthMethods(),
	}, nil
}

// handleAuthenticate handles the inbound `authenticate` request.
// Validates the token via AuthGate and unblocks scope-gated handlers.
// Returns null on success per the spec.
func (a *Adapter) handleAuthenticate(_ context.Context, params json.RawMessage) (any, error) {
	var p AuthenticateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "authenticate params: " + err.Error()}
	}
	if rpcErr := a.auth.Authenticate(p); rpcErr != nil {
		return nil, rpcErr
	}
	return nil, nil
}

// handleSessionNew creates a new mux session for the editor's CWD.
// Returns the session ID. Editor-supplied mcpServers are ignored per
// v005-09 §2 lock (boot-profile MCPs apply); the raw payload is logged.
func (a *Adapter) handleSessionNew(_ *Dispatcher) HandlerFunc {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if rpcErr := a.auth.HasScope(ScopeSessionWrite); rpcErr != nil {
			return nil, rpcErr
		}
		var p NewSessionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/new params: " + err.Error()}
		}
		if p.CWD == "" {
			return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/new: cwd required"}
		}
		if len(p.McpServers) > 0 {
			a.logger.Warn("acp: editor-supplied mcpServers ignored (using boot-profile MCPs); see followup_acp_editor_mcp_passthrough",
				"count", len(p.McpServers))
		}

		id, err := a.svc.LaunchSession(ctx, LaunchInput{
			CWD:              p.CWD,
			EditorMcpServers: p.McpServers,
		})
		if err != nil {
			return nil, mapServiceError(err)
		}
		a.trackOwned(id)
		return NewSessionResult{SessionID: id}, nil
	}
}

// handleSessionPrompt drives one prompt turn. Streams agent output
// into outbound `session/update` notifications and returns when the
// turn completes with the appropriate stop reason.
func (a *Adapter) handleSessionPrompt(d *Dispatcher) HandlerFunc {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if rpcErr := a.auth.HasScope(ScopeSessionWrite); rpcErr != nil {
			return nil, rpcErr
		}
		var p PromptParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/prompt params: " + err.Error()}
		}
		if p.SessionID == "" {
			return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/prompt: sessionId required"}
		}
		// Validate prompt content variants against advertised
		// capabilities. Text + resource_link allowed; image/audio/
		// resource rejected per v005-09 capability lock.
		for i, blk := range p.Prompt {
			switch blk.Type {
			case ContentTypeText, ContentTypeResourceLink:
				// ok
			case ContentTypeImage, ContentTypeAudio, ContentTypeResource:
				return nil, &RPCError{
					Code:    ErrCodeInvalidParams,
					Message: fmt.Sprintf("session/prompt: prompt[%d].type=%q not supported (text-only MVP)", i, blk.Type),
				}
			default:
				return nil, &RPCError{
					Code:    ErrCodeInvalidParams,
					Message: fmt.Sprintf("session/prompt: prompt[%d].type=%q unknown", i, blk.Type),
				}
			}
		}

		updates, err := a.svc.SendTurn(ctx, p.SessionID, p.Prompt)
		if err != nil {
			return nil, mapServiceError(err)
		}

		stopReason := StopReasonEndTurn
		for upd := range updates {
			switch upd.Kind {
			case TurnUpdateKindText:
				notif := SessionUpdateNotification{
					SessionID: p.SessionID,
					Update: SessionUpdate{
						SessionUpdate: SessionUpdateAgentMessageChunk,
						Content:       &ContentBlock{Type: ContentTypeText, Text: upd.Text},
					},
				}
				if err := d.Notify("session/update", notif); err != nil {
					a.logger.Warn("acp: session/update emit failed", "err", err)
				}
			case TurnUpdateKindDone:
				if upd.StopReason != "" {
					stopReason = upd.StopReason
				}
			}
		}
		return PromptResult{StopReason: stopReason}, nil
	}
}

// handleSessionCancel processes the `session/cancel` notification by
// signaling the host to interrupt the in-flight turn for that session.
// Notifications are fire-and-forget; errors are logged.
func (a *Adapter) handleSessionCancel(ctx context.Context, params json.RawMessage) {
	var p CancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		a.logger.Warn("acp: session/cancel parse", "err", err)
		return
	}
	if p.SessionID == "" {
		a.logger.Warn("acp: session/cancel missing sessionId")
		return
	}
	if err := a.svc.CancelTurn(ctx, p.SessionID); err != nil {
		a.logger.Warn("acp: cancel turn", "session_id", p.SessionID, "err", err)
	}
}

// handleSessionClose terminates the session and drops it from the
// connection's owned set. Returns an empty object on success.
func (a *Adapter) handleSessionClose(ctx context.Context, params json.RawMessage) (any, error) {
	if rpcErr := a.auth.HasScope(ScopeSessionWrite); rpcErr != nil {
		return nil, rpcErr
	}
	var p CloseSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/close params: " + err.Error()}
	}
	if p.SessionID == "" {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/close: sessionId required"}
	}
	if err := a.svc.CloseSession(ctx, p.SessionID); err != nil {
		return nil, mapServiceError(err)
	}
	a.untrackOwned(p.SessionID)
	return CloseSessionResult{}, nil
}

// handleSessionResume reattaches to an existing mux session by ID.
// Used for multi-client attach (a second editor joins a session that
// was created by the first). Editor-supplied mcpServers are ignored
// per v005-09 §2 lock.
func (a *Adapter) handleSessionResume(ctx context.Context, params json.RawMessage) (any, error) {
	if rpcErr := a.auth.HasScope(ScopeSessionWrite); rpcErr != nil {
		return nil, rpcErr
	}
	var p ResumeSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/resume params: " + err.Error()}
	}
	if p.SessionID == "" {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "session/resume: sessionId required"}
	}
	if len(p.McpServers) > 0 {
		a.logger.Warn("acp: editor-supplied mcpServers ignored on resume",
			"session_id", p.SessionID, "count", len(p.McpServers))
	}
	if err := a.svc.ResumeSession(ctx, p.SessionID, p.CWD); err != nil {
		return nil, mapServiceError(err)
	}
	a.trackOwned(p.SessionID)
	return ResumeSessionResult{}, nil
}

// mapServiceError translates Service-layer sentinel errors into
// JSON-RPC error envelopes. Other errors land as ErrCodeInternal.
func mapServiceError(err error) *RPCError {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return &RPCError{Code: ErrCodeInvalidParams, Message: err.Error()}
	default:
		return &RPCError{Code: ErrCodeInternal, Message: err.Error()}
	}
}
