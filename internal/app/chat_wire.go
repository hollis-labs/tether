package app

import (
	"fmt"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/chatbackend/agystdio"
	"github.com/hollis-labs/tether/internal/chatbackend/claudepty"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
	"github.com/hollis-labs/tether/internal/chatbackend/claudetranscript"
	"github.com/hollis-labs/tether/internal/chatwire"
	"github.com/hollis-labs/tether/internal/config"
)

// The chat wire's view of sessions (CW-20261118-0011): *Service is chatwire.Sessions. The runner picks the backend
// by runtime and runs it for as long as it serves the session.

var _ chatwire.Sessions = (*Service)(nil)

// ChatRuntime implements chatwire.Sessions: the session's provider brand and runtime kind from its stored launch plan,
// and for a local session the runtime's own capabilities, which win.
func (s *Service) ChatRuntime(id string) (chatwire.Runtime, error) {
	plan, err := s.Store.GetLaunchPlan(id)
	if err != nil {
		return "", fmt.Errorf("session %s: launch plan: %w", id, err)
	}
	if plan == nil {
		return "", &chatwire.UnsupportedRuntimeError{SessionID: id, Reason: "no launch plan"}
	}
	kind := config.ParseRuntimeKind(plan.RuntimeKind)
	if s.Manager != nil {
		if info, ok := s.Manager.Get(id); ok {
			switch {
			case info.Caps.PTY:
				kind = config.RuntimeKindPTY
			case info.Caps.StreamingStdio:
				kind = config.RuntimeKindStreamingStdio
			case info.Caps.JsonRpcStdio:
				kind = config.RuntimeKindJSONRPCStdio
			}
		}
	}
	brand := config.CanonicalRuntimeID(plan.ProviderBrand)
	switch {
	case claudeBrand(plan.ProviderBrand) && kind == config.RuntimeKindStreamingStdio:
		return chatwire.RuntimeClaudeStdio, nil
	case claudeBrand(plan.ProviderBrand) && kind == config.RuntimeKindPTY:
		return chatwire.RuntimeClaudePTY, nil
	case brand == "antigravity" && (kind == config.RuntimeKindSubprocess || kind == config.RuntimeKindStreamingStdio):
		return chatwire.RuntimeAgyStdio, nil
	case brand == "codex" && kind == config.RuntimeKindJSONRPCStdio:
		return chatwire.RuntimeCodexAppServer, nil
	}
	return "", &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: chatwire.Runtime(brand + "/" + kind), Reason: "no chat backend for this provider and runtime"}
}

// ChatBackend implements chatwire.Sessions.
func (s *Service) ChatBackend(id string, rt chatwire.Runtime) (chatwire.Backend, error) {
	switch rt {
	case chatwire.RuntimeAgyStdio:
		if v, ok := s.agyExecutors.Load(id); ok {
			if s.stdioTap(id) != nil && !s.agyChatControlSupported(id) {
				return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: rt, Reason: "retained agy host lacks correlated chat controls; start a new seat"}
			}
			return agystdio.New(agystdio.Config{Executor: v.(agystdio.Executor)}), nil
		}
		return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: rt, Reason: "agy chat requires a local per-turn runtime launched by this daemon; worker seats are not supported"}

	case chatwire.RuntimeClaudeStdio:
		b, err := s.ClaudeStdioBackend(id, claudestdio.Config{})
		if err != nil {
			return nil, err
		}
		return b, nil
	case chatwire.RuntimeClaudePTY:
		return s.claudePTYBackend(id)
	case chatwire.RuntimeCodexAppServer:
		// TODO(CW-20261118-0009): the codex app-server chat backend.
		return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: rt, Reason: "the codex app-server backend is CW-20261118-0009"}
	}
	return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: rt, Reason: "no chat backend for this runtime"}
}

// claudePTYBackend is the PTY backend for a local Claude TUI session: keystrokes through the Manager, content from
// the vendor transcript under this daemon's Claude projects directory. A worker session's transcript is on the worker,
// so it is not served yet. Hooks are not wired: the hub plants none for local sessions, and the backend then ends runs
// on the transcript's own markers.
func (s *Service) claudePTYBackend(id string) (chatwire.Backend, error) {
	if _, worker := s.workerSession(id); worker {
		return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: chatwire.RuntimeClaudePTY, Reason: "a worker session's transcript is not on this host"}
	}
	if s.Manager == nil {
		return nil, agentsessions.ErrSessionNotRunning
	}
	if info, ok := s.Manager.Get(id); !ok || !info.Caps.PTY {
		return nil, agentsessions.ErrSessionNotRunning
	}
	dir, err := claudetranscript.ProjectsDir()
	if err != nil {
		return nil, err
	}
	var b *claudepty.Backend
	cfg := claudepty.Config{SessionID: id, Input: s.Manager, ClaudeSessionID: s.claudeNativeSession(id)}
	cfg.Transcript = claudepty.SessionTranscript(dir, func() string { return b.ClaudeSessionID() })
	b = claudepty.New(cfg)
	return b, nil
}

// claudeNativeSession is the Claude session id Tether recorded for a session, if any.
func (s *Service) claudeNativeSession(id string) string {
	maps, err := s.Store.ListSessionProviderMappings(id)
	if err != nil {
		return ""
	}
	for _, m := range maps {
		if claudeBrand(m.Provider) && m.NativeSessionID.Valid {
			return m.NativeSessionID.String
		}
	}
	return ""
}

// Terminal attachments (CW-20261118-0012): *Service is chatwire.TerminalSessions.
var _ chatwire.TerminalSessions = (*Service)(nil)

// HasTerminal implements chatwire.TerminalSessions: the Manager runs a live PTY for the session on this host. It reads
// only the Manager's in-process table; terminal attachments call it under the chat host's lease lock.
func (s *Service) HasTerminal(id string) bool {
	if s == nil || s.Manager == nil {
		return false
	}
	info, ok := s.Manager.Get(id)
	return ok && info.Caps.PTY
}
