package stub

import (
	"context"
	"io"
	"sync"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/chrispian/agent-mux/internal/launch"
)

// New constructs an agentsessions.Runtime that runs entirely in-process:
// SendInput appends "echo: " + data + "\n" to the fanout writer; Wait
// blocks until Stop. Useful for tests and as a contract reference. plan
// is ignored (in-process providers don't read launch policy).
func New(_ *launch.Plan) (agentsessions.Runtime, error) {
	return &runtime{}, nil
}

type runtime struct{}

func (r *runtime) ID() string   { return "api-stub" }
func (r *runtime) Kind() string { return "api" }
func (r *runtime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: false,
		CheckpointResume:  false,
		BinaryRequired:    false,
	}
}

func (r *runtime) Prepare(_ context.Context) error { return nil }

func (r *runtime) Start(_ context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	s := &echoSession{fanout: opts.Fanout, done: make(chan struct{})}
	if opts.BootMode == "stdin" && opts.BootPrompt != "" {
		_ = s.writeEcho([]byte(opts.BootPrompt))
	}
	return s, nil
}

type echoSession struct {
	fanout io.Writer

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func (s *echoSession) Wait() (int, error) {
	<-s.done
	return 0, nil
}

func (s *echoSession) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *echoSession) SendInput(_ context.Context, data []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return agentsessions.ErrNoInputChannel
	}
	return s.writeEcho(data)
}

// writeEcho formats data as "echo: <data>\n" through the fanout writer.
// A trailing newline is added unless data already ends with one. A nil
// fanout is a silent no-op.
func (s *echoSession) writeEcho(data []byte) error {
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

func (s *echoSession) Resize(_ context.Context, _, _ uint16) error { return nil }

func (s *echoSession) Health() agentsessions.HealthStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := agentsessions.LiveStateIdle
	if s.closed {
		state = agentsessions.LiveStateStopped
	}
	return agentsessions.HealthStatus{Alive: !s.closed, PID: 0, State: state}
}

func (s *echoSession) CheckpointHints() (agentsessions.CheckpointHint, bool) {
	return nil, false
}

var (
	_ agentsessions.Runtime = (*runtime)(nil)
	_ agentsessions.Session = (*echoSession)(nil)
)
