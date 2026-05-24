package app

import (
	"context"
	"fmt"

	"github.com/hollis-labs/go-agent-runtime/runtimekind"
	"github.com/hollis-labs/go-agent-runtime/turn"
	"github.com/hollis-labs/go-agent-sessions/agentsessions"
)

// SendInput writes data to the named session's input channel. Thin wrapper
// over agentsessions.Manager.SendInput.
func (s *Service) SendInput(id string, data []byte) error {
	return s.Manager.SendInput(id, data)
}

// SendTurn delivers a user message to the named session, applying the
// per-runtime framing required by the session's lifecycle mode:
//
//   - StreamingStdio (Claude): wraps text as a JSON user-message frame:
//     {"type":"user","message":{"role":"user","content":"<text>"}}
//     and writes it to stdin. The session runtime owns line delimiting.
//   - JsonRpcStdio (Codex app-server): performs lazy initialize +
//     thread/start on first call (caching the thread id per session id),
//     then issues turn/start with the cached thread id and the text input.
//   - PTY or unknown: falls back to raw SendInput([]byte(text)) so PTY
//     consumers still work without per-call framing.
//
// Existing SendInput callers are unaffected — SendTurn is additive and
// intended for callers that want lifecycle-aware framing without
// hand-rolling the per-mode envelope.
func (s *Service) SendTurn(ctx context.Context, id, text string) error {
	info, ok := s.Manager.Get(id)
	if !ok {
		return agentsessions.ErrSessionNotRunning
	}
	switch {
	case info.Caps.StreamingStdio:
		payload, err := frameUserMessage(text)
		if err != nil {
			return err
		}
		return s.Manager.SendInput(id, payload)
	case info.Caps.JsonRpcStdio:
		return s.sendTurnJSONRPC(ctx, id, text)
	default:
		return s.Manager.SendInput(id, []byte(text))
	}
}

// frameUserMessage encodes the JSON user-message frame Claude's
// streaming-input mode expects on stdin. The envelope shape is
// {"type":"user","message":{"role":"user","content":"<text>"}}.
// The underlying session runtime adds the line delimiter on write.
func frameUserMessage(text string) ([]byte, error) {
	payload, err := turn.Frame(text, turn.Options{
		Provider: "claude",
		Runtime:  runtimekind.StreamingStdio,
	})
	if err != nil {
		return nil, fmt.Errorf("encode user message: %w", err)
	}
	return payload, nil
}

// ResizeSession forwards a (rows, cols) winsize update. Thin wrapper over
// agentsessions.Manager.Resize.
func (s *Service) ResizeSession(id string, rows, cols uint16) error {
	return s.Manager.Resize(id, rows, cols)
}
