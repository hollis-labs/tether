package antigravity

import (
	"context"
	"errors"
	"sync"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	events "github.com/hollis-labs/libs/harness/core/adapters/provider/events"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

// chatRuntime keeps the seat's existing runtime in charge of every subprocess.
// A chat turn borrows the same session and raw parser, never spawning a parallel
// process outside the launch's sandbox or reconstructing its environment.
type chatRuntime struct {
	agentsessions.Runtime
	mu      sync.Mutex
	session *chatSession
	line    func([]byte)
}

func (r *chatRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	s, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	wrapped := &chatSession{Session: s}
	r.mu.Lock()
	r.session = wrapped
	r.mu.Unlock()
	return wrapped, nil
}

// StartTurn reserves against both chat and ordinary session input. Context goes
// directly to Session.SendInput, so cancel ends only this subprocess, not the seat.
func (r *chatRuntime) StartTurn(ctx context.Context, text string, line func([]byte)) (func() error, error) {
	r.mu.Lock()
	s := r.session
	if s == nil || !s.Health().Alive {
		r.mu.Unlock()
		return nil, chatbackend.ErrClosed
	}
	if !s.input.TryLock() {
		r.mu.Unlock()
		return nil, chatbackend.ErrBusy
	}
	r.line = line
	expected := s.ProviderSessionID()
	r.mu.Unlock()
	return func() error {
		defer s.input.Unlock()
		defer func() { r.mu.Lock(); r.line = nil; r.mu.Unlock() }()
		err := s.Session.SendInput(ctx, []byte(text))
		if err == nil && expected != "" && s.ProviderSessionID() != expected {
			return errors.New("agy native conversation replaced")
		}
		return err
	}, nil
}

type chatAdapter struct {
	*gop.AntigravityAdapter
	bridge *chatRuntime
}

// The adapter runtime invokes the typed parser once before its legacy parser.
// Hook only this path so a line is never decoded twice by chatstream.
func (a *chatAdapter) ParseLineEvents(raw []byte) ([]events.Event, error) {
	a.bridge.mu.Lock()
	if a.bridge.line != nil {
		a.bridge.line(raw)
	}
	a.bridge.mu.Unlock()
	return a.AntigravityAdapter.ParseLineEvents(raw)
}

type chatSession struct {
	agentsessions.Session
	input sync.Mutex
}

func (s *chatSession) SendInput(ctx context.Context, data []byte) error {
	if !s.input.TryLock() {
		return agentsessions.ErrTurnInFlight
	}
	defer s.input.Unlock()
	return s.Session.SendInput(ctx, data)
}

// Preserve the adapter session's optional lifecycle evidence interfaces.
func (s *chatSession) ProviderSessionID() string {
	return s.Session.(agentsessions.SessionIDer).ProviderSessionID()
}
func (s *chatSession) SandboxOutcome() (agentsessions.SandboxOutcome, bool) {
	return s.Session.(agentsessions.SandboxOutcomeReporter).SandboxOutcome()
}
func (s *chatSession) LivePID() int {
	if p, ok := s.Session.(agentsessions.PIDReporter); ok {
		return p.LivePID()
	}
	return s.Health().PID
}
func (s *chatSession) LastPID() int {
	if p, ok := s.Session.(agentsessions.PIDReporter); ok {
		return p.LastPID()
	}
	return 0
}
