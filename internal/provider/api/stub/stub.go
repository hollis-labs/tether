// Package stub is a deliberately throwaway API-runtime implementation. It
// exists in v0.0.2 to prove the provider.Runtime / provider.Session contract
// fits a non-process runtime shape — so the upcoming real API providers
// (Anthropic, OpenAI) in v0.0.3 plug in without interface churn.
//
// The stub runs entirely in-process: SendInput appends "echo: " + data + "\n"
// to the fan-out writer supplied at Start time; Wait blocks until Stop is
// called. There is no persistent state, no retry, no streaming semantics
// beyond the single-round-trip echo. Delete this package once the first
// real API provider lands.
package stub

import (
	"context"
	"io"
	"sync"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
)

// Runtime is the api-stub provider. It satisfies provider.Runtime and
// returns a *Session on Start.
type Runtime struct{}

func (Runtime) ID() string                 { return "api-stub" }
func (Runtime) Kind() provider.RuntimeKind { return provider.RuntimeKindAPI }

// Prepare is a no-op — the stub has no validation to perform. Real API
// runtimes will use Prepare to verify credentials, model availability, etc.
func (Runtime) Prepare(_ context.Context, _ *launch.Plan) error { return nil }

func (Runtime) Start(_ context.Context, _ *launch.Plan, opts provider.StartOptions) (provider.Session, error) {
	s := &Session{fanout: opts.Fanout, done: make(chan struct{})}
	// Mirror stdin-mode boot prompts through the echo pipeline so attached
	// clients see the initial state just like a CLI session would.
	if opts.BootMode == "stdin" && opts.BootPrompt != "" {
		_ = s.writeEcho([]byte(opts.BootPrompt))
	}
	return s, nil
}

// Session is the api-stub's Session implementation. It keeps the fan-out
// writer supplied at Start and drives the echo flow through it.
type Session struct {
	fanout io.Writer

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func (s *Session) Wait() (int, error) {
	<-s.done
	return 0, nil
}

func (s *Session) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *Session) SendInput(_ context.Context, data []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return provider.ErrNoInputChannel
	}
	return s.writeEcho(data)
}

// writeEcho formats data as "echo: <data>\n" and writes it through the
// fan-out writer. If data already ends with '\n' the trailing newline is
// not duplicated. A nil fanout is a silent no-op (used by tests that don't
// care about the echoed output).
func (s *Session) writeEcho(data []byte) error {
	if s.fanout == nil {
		return nil
	}
	if _, err := s.fanout.Write([]byte("echo: ")); err != nil {
		return err
	}
	if _, err := s.fanout.Write(data); err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		if _, err := s.fanout.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

// Resize is a no-op for the stub API provider — there's no PTY to
// propagate to. Satisfies the provider.Session contract added for
// ADR 0014.
func (s *Session) Resize(_ context.Context, _, _ uint16) error { return nil }

func (s *Session) Health() provider.HealthStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return provider.HealthStatus{Alive: !s.closed, PID: 0}
}

func (s *Session) CheckpointHints() (provider.CheckpointHint, bool) {
	return provider.CheckpointHint{}, false
}

// Static interface checks. If these fail to compile, the Runtime / Session
// contracts have drifted and the stub must be updated.
var (
	_ provider.Runtime = Runtime{}
	_ provider.Session = (*Session)(nil)
)
