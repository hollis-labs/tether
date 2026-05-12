// Package acpsvc bridges internal/acpadapter.Service to the running mux
// daemon over UDS. This is the mux-specific glue intentionally kept
// outside internal/acpadapter so that package can extract cleanly to a
// portfolio go-acp library (see Vanta `followup_portfolio_go_acp_extraction`).
//
// The Service maps each ACP session to one daemon-launched mux session
// and one long-lived attach goroutine that parses the claudestream
// NDJSON output. SendTurn registers a per-turn delta channel; the
// attach goroutine routes deltas to the active turn's channel and
// signals completion when claudestream emits KindDone.
package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/chrispian/agent-mux/internal/acpadapter"
	"github.com/chrispian/agent-mux/internal/client"
	"github.com/chrispian/agent-mux/pkg/claudestream"
)

// Service implements acpadapter.Service against a running mux daemon.
type Service struct {
	client   *client.Client
	launchID string
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[acpadapter.SessionID]*sessionState
}

// New constructs a Service that creates sessions via launchID against
// the daemon at dc. launchID is the mux launch profile name passed to
// `mux acp --agent <launch_id>`. logger is used for warn-level events
// during attach/parse — wire to stderr so it doesn't pollute the ACP
// stdout protocol stream.
func New(dc *client.Client, launchID string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		client:   dc,
		launchID: launchID,
		logger:   logger,
		sessions: map[acpadapter.SessionID]*sessionState{},
	}
}

// sessionState tracks per-session attach + per-turn routing.
type sessionState struct {
	id acpadapter.SessionID

	// attachCancel stops the long-lived attach goroutine on close.
	attachCancel context.CancelFunc

	// attachDone closes when the attach goroutine exits, so CloseSession
	// can wait for clean teardown.
	attachDone chan struct{}

	// turnMu guards currentTurn. Held briefly to register/unregister.
	turnMu      sync.Mutex
	currentTurn *turnState
}

// turnState carries the channel + cancel for one in-flight prompt turn.
type turnState struct {
	updates chan acpadapter.TurnUpdate
	// canceled is set by CancelTurn so the attach loop knows to emit
	// StopReasonCancelled when the underlying agent eventually finishes.
	// Best-effort: there's no interrupt primitive in the daemon yet, so
	// the agent continues generating; we just stop forwarding to the
	// editor and report the right stop reason.
	canceled bool
}

// LaunchSession creates a new mux session via the configured launch
// profile and starts the long-lived attach goroutine that drives
// streaming output for subsequent turns.
//
// MVP simplification: the editor's CWD is logged but does NOT override
// the launch profile's workspace strategy (typically tmpdir). Per-turn
// CWD respect is captured as `followup_acp_session_cwd_workspace_override`.
func (s *Service) LaunchSession(ctx context.Context, params acpadapter.LaunchInput) (acpadapter.SessionID, error) {
	if s.client == nil {
		return "", acpadapter.ErrUnsupported
	}
	if s.launchID == "" {
		return "", fmt.Errorf("acpsvc: no launch profile configured (pass --agent on `mux acp`)")
	}
	if params.CWD != "" {
		s.logger.Info("acp: session/new cwd recorded but launch profile workspace applies",
			"cwd", params.CWD)
	}

	resp, err := s.client.Launch(ctx, s.launchID)
	if err != nil {
		return "", fmt.Errorf("acpsvc: launch %q: %w", s.launchID, err)
	}
	id := acpadapter.SessionID(resp.ID)

	state := s.startAttach(id)
	s.mu.Lock()
	s.sessions[id] = state
	s.mu.Unlock()

	return id, nil
}

// startAttach spawns the long-lived per-session attach goroutine that
// pipes daemon output through a claudestream.Scanner and routes typed
// events to whichever turn is currently active (if any).
func (s *Service) startAttach(id acpadapter.SessionID) *sessionState {
	attachCtx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancel is stored on state.attachCancel and invoked by CloseSession
	state := &sessionState{
		id:           id,
		attachCancel: cancel,
		attachDone:   make(chan struct{}),
	}

	go func() {
		defer close(state.attachDone)

		pr, pw := io.Pipe()

		// Goroutine A: read attach stream from daemon, copy bytes
		// into the pipe writer. Closes the pipe when attach exits.
		attachErr := make(chan error, 1)
		go func() {
			err := s.client.AttachSession(attachCtx, string(id), pw, 0)
			_ = pw.CloseWithError(err)
			attachErr <- err
		}()

		// Goroutine B (this one): parse the pipe reader as claudestream
		// events and route into the current turn's update channel.
		scanner := claudestream.NewScanner(pr)
		for {
			ev, ok, err := scanner.Next()
			if err != nil {
				s.logger.Warn("acp: claudestream parse error", "session_id", id, "err", err)
				continue
			}
			if !ok {
				// Clean EOF — attach closed.
				break
			}
			s.routeEvent(state, ev)
		}

		// Drain attach error so the goroutine exits cleanly.
		_ = pr.Close()
		<-attachErr

		// If a turn was in flight when attach died, signal it with
		// StopReasonCancelled so the handler unblocks instead of
		// hanging forever.
		state.turnMu.Lock()
		if state.currentTurn != nil {
			state.finishTurnLocked(acpadapter.StopReasonCancelled)
		}
		state.turnMu.Unlock()
	}()

	return state
}

// routeEvent fans one claudestream event into the active turn (if any)
// or drops it (e.g. boot greeting before the first SendTurn).
func (s *Service) routeEvent(state *sessionState, ev claudestream.Event) {
	state.turnMu.Lock()
	defer state.turnMu.Unlock()

	turn := state.currentTurn
	if turn == nil {
		// No turn active — drop. This is normal for the
		// session greeting between launch and the first SendTurn.
		return
	}

	switch ev.Kind {
	case claudestream.KindDelta:
		select {
		case turn.updates <- acpadapter.TurnUpdate{
			Kind: acpadapter.TurnUpdateKindText,
			Text: ev.Text,
		}:
		default:
			// Channel full or closed — handler is wrapping up; drop.
		}

	case claudestream.KindDone:
		stopReason := acpadapter.StopReasonEndTurn
		if turn.canceled {
			stopReason = acpadapter.StopReasonCancelled
		}
		state.finishTurnLocked(stopReason)

	case claudestream.KindError:
		// Treat top-level errors as a refusal-class signal. The
		// handler returns refusal stop reason; the editor can
		// surface to the user.
		s.logger.Warn("acp: claudestream error", "session_id", state.id, "err", ev.ErrorMsg)
		state.finishTurnLocked(acpadapter.StopReasonRefusal)

	case claudestream.KindUsage, claudestream.KindToolUse,
		claudestream.KindUIPrompt, claudestream.KindSessionID:
		// Non-fatal informational events; not surfaced as ACP
		// updates MVP. Tool calls are deferred per v005-09 §5.
	}
}

// finishTurnLocked emits the terminal Done update, closes the channel,
// and clears currentTurn. Caller must hold state.turnMu.
func (state *sessionState) finishTurnLocked(reason acpadapter.StopReason) {
	if state.currentTurn == nil {
		return
	}
	turn := state.currentTurn
	state.currentTurn = nil
	// Best-effort send + close. If the handler already gave up on
	// the chan (ctx canceled, etc.) the recv side is gone but the
	// channel's still open — close is safe.
	select {
	case turn.updates <- acpadapter.TurnUpdate{
		Kind:       acpadapter.TurnUpdateKindDone,
		StopReason: reason,
	}:
	default:
	}
	close(turn.updates)
}

// SendTurn registers a fresh turn channel and kicks the daemon. Returns
// the channel for the dispatcher to drain. The attach goroutine routes
// claudestream deltas into the channel; finishTurnLocked emits the
// terminal Done update and closes the channel when the turn ends.
func (s *Service) SendTurn(ctx context.Context, id acpadapter.SessionID, prompt []acpadapter.ContentBlock) (<-chan acpadapter.TurnUpdate, error) {
	state, err := s.getSession(id)
	if err != nil {
		return nil, err
	}

	// Concatenate text content blocks into the wire-level user message.
	// Per v005-09 capability lock we only accept text + resource_link;
	// the handler already validated, so any non-text here would be a bug.
	text := concatTextBlocks(prompt)

	state.turnMu.Lock()
	if state.currentTurn != nil {
		state.turnMu.Unlock()
		return nil, fmt.Errorf("acpsvc: a turn is already in flight on session %s", id)
	}
	turn := &turnState{
		updates: make(chan acpadapter.TurnUpdate, 16),
	}
	state.currentTurn = turn
	state.turnMu.Unlock()

	if err := s.client.SendTurn(ctx, string(id), text); err != nil {
		// Roll back the registration so the session can accept a retry.
		state.turnMu.Lock()
		if state.currentTurn == turn {
			state.currentTurn = nil
		}
		state.turnMu.Unlock()
		close(turn.updates)
		return nil, fmt.Errorf("acpsvc: send turn: %w", err)
	}
	return turn.updates, nil
}

// CancelTurn marks the current turn as canceled so the eventual
// claudestream Done event maps to StopReasonCancelled. Best-effort —
// see followup_acp_cancel_underlying_interrupt.
func (s *Service) CancelTurn(_ context.Context, id acpadapter.SessionID) error {
	state, err := s.getSession(id)
	if err != nil {
		return err
	}
	state.turnMu.Lock()
	defer state.turnMu.Unlock()
	if state.currentTurn != nil {
		state.currentTurn.canceled = true
	}
	return nil
}

// CloseSession stops the daemon-side session and cancels the per-session
// attach goroutine. Idempotent: closing an already-closed session
// returns nil.
func (s *Service) CloseSession(ctx context.Context, id acpadapter.SessionID) error {
	s.mu.Lock()
	state, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}

	state.attachCancel()
	<-state.attachDone

	if err := s.client.StopSession(ctx, string(id)); err != nil {
		// Daemon may have already cleaned up the session; treat
		// not-found as success on close.
		if errors.Is(err, client.ErrDaemonUnreachable) {
			return err
		}
		// Other errors logged but not propagated — close is
		// best-effort.
		s.logger.Warn("acp: stop session on close", "session_id", id, "err", err)
	}
	return nil
}

// ResumeSession reattaches to an existing mux session ID. Multi-client
// scenario: another ACP connection (or the original) launched the
// session; this connection joins by ID and starts receiving its
// streaming output.
func (s *Service) ResumeSession(_ context.Context, id acpadapter.SessionID, cwd string) error {
	if cwd != "" {
		s.logger.Info("acp: session/resume cwd recorded but ignored",
			"session_id", id, "cwd", cwd)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[id]; exists {
		// Already attached on this connection; idempotent.
		return nil
	}
	// Verify the session exists on the daemon before starting an
	// attach goroutine that would otherwise block forever.
	if _, err := s.client.GetSession(context.Background(), string(id)); err != nil {
		return acpadapter.ErrSessionNotFound
	}
	state := s.startAttach(id)
	s.sessions[id] = state
	return nil
}

// getSession returns the per-session state or ErrSessionNotFound.
func (s *Service) getSession(id acpadapter.SessionID) (*sessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[id]
	if !ok {
		return nil, acpadapter.ErrSessionNotFound
	}
	return state, nil
}

// concatTextBlocks joins the text fields of all text-typed blocks. Other
// block types (resource_link MVP) contribute nothing to the wire-level
// turn payload — they're metadata for the agent's context, not user text.
func concatTextBlocks(prompt []acpadapter.ContentBlock) string {
	if len(prompt) == 1 && prompt[0].Type == acpadapter.ContentTypeText {
		return prompt[0].Text
	}
	out := ""
	for _, blk := range prompt {
		if blk.Type == acpadapter.ContentTypeText {
			if out != "" {
				out += "\n\n"
			}
			out += blk.Text
		}
	}
	return out
}
