package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

// Chat taps for Claude streaming-stdio sessions (CW-20261118-0010). A claudestdio.Tap sees a session's stdout lines
// and the user frames written to its stdin, which is what claudestdio.Backend runs chat turns from. The taps add no
// consumer of their own and change no existing one: they copy what already flows.
//
//   - Local sessions: the tap is the runtime's output Fanout (agentsessions.StartOptions.Fanout, which the
//     streaming-stdio reader writes every stdout line to, next to the session log), installed at launch and at shim
//     reattach.
//   - Worker sessions: accepted "output" frames on the stdout stream (workerTurnOutput, in order, after the commit),
//     once a backend has asked for the session's tap.
//   - Stdin, both: SendTurn and SendInput note their frames to the tap before writing (stdioInput).

// Labels for the frames other writers put on stdin; an observed run carries them as Origin.
const (
	stdioFromSendTurn  = "send_turn"
	stdioFromSendInput = "send_input"
)

func claudeBrand(brand string) bool { return brand == "claude" || brand == "claude-code" }

// installStdioTap makes a local Claude streaming-stdio session's stdout reach a new tap and returns it (nil for any
// other session).
func (s *Service) installStdioTap(id, brand string, rt agentsessions.Runtime, opts *agentsessions.StartOptions) *claudestdio.Tap {
	if !claudeBrand(brand) || rt == nil || !rt.Caps().StreamingStdio {
		return nil
	}
	tap := claudestdio.NewTap()
	if opts.Fanout == nil {
		opts.Fanout = tap
	} else {
		opts.Fanout = io.MultiWriter(opts.Fanout, tap)
	}
	if old, loaded := s.stdioTaps.Swap(id, tap); loaded {
		old.(*claudestdio.Tap).Close()
	}
	return tap
}

// watchStdioTap closes a local session's tap when the session ends.
func (s *Service) watchStdioTap(id string, tap *claudestdio.Tap) {
	if tap == nil {
		return
	}
	go func() {
		_, _ = s.Manager.WaitSession(context.Background(), id)
		s.dropStdioTap(id, tap)
	}()
}

// dropStdioTap closes the session's tap (only tap, when it is not nil) and forgets it.
func (s *Service) dropStdioTap(id string, tap *claudestdio.Tap) {
	v, ok := s.stdioTaps.Load(id)
	if !ok || (tap != nil && v.(*claudestdio.Tap) != tap) {
		return
	}
	if s.stdioTaps.CompareAndDelete(id, v) {
		v.(*claudestdio.Tap).Close()
	}
}

func (s *Service) stdioTap(id string) *claudestdio.Tap {
	if v, ok := s.stdioTaps.Load(id); ok {
		return v.(*claudestdio.Tap)
	}
	return nil
}

// feedWorkerStdioTap passes an accepted worker stdout frame to the session's tap, if a backend asked for one.
func (s *Service) feedWorkerStdioTap(id string, data []byte) {
	if tap := s.stdioTap(id); tap != nil {
		_, _ = tap.Write(data)
	}
}

// stdioInput writes data to a session's stdin through write, noting its user frames to the session's tap first so
// the chat backend can tell whose turn starts next. Without a tap it is just write.
func (s *Service) stdioInput(id string, data []byte, from string, write func() error) error {
	tap := s.stdioTap(id)
	if tap == nil {
		return write()
	}
	return tap.Input(data, from, write)
}

// ClaudeStdioBackend returns a chat backend for a running Claude streaming-stdio session, local or worker. cfg's Tap
// and Stdin are filled in; the caller runs it (Backend.Run) for as long as it serves the chat.
func (s *Service) ClaudeStdioBackend(id string, cfg claudestdio.Config) (*claudestdio.Backend, error) {
	if row, worker := s.workerSession(id); worker {
		plan, err := s.Store.GetLaunchPlan(id)
		if err != nil {
			return nil, err
		}
		if !claudeBrand(plan.ProviderBrand) {
			return nil, fmt.Errorf("session %s is not a Claude session", id)
		}
		if row.State != "running" && row.State != "launching" {
			return nil, agentsessions.ErrSessionNotRunning
		}
		v, _ := s.stdioTaps.LoadOrStore(id, claudestdio.NewTap())
		cfg.Tap = v.(*claudestdio.Tap)
	} else {
		if s.Manager == nil {
			return nil, agentsessions.ErrSessionNotRunning
		}
		info, ok := s.Manager.Get(id)
		if !ok {
			return nil, agentsessions.ErrSessionNotRunning
		}
		if !info.Caps.StreamingStdio {
			return nil, fmt.Errorf("session %s is not a streaming-stdio session", id)
		}
		cfg.Tap = s.stdioTap(id)
		if cfg.Tap == nil {
			return nil, errors.New("session has no chat tap (not a Claude session, or launched by an older daemon)")
		}
	}
	cfg.Stdin = stdioWriter{s: s, id: id}
	return claudestdio.New(cfg), nil
}

// stdioWriter is the chat backend's stdin: the session's own write path, without the tap (the backend notes its
// frames itself). A user frame is a turn input like SendTurn's (who asked is recorded; a local one is tracked as a
// turn submission); a control request is written as is.
type stdioWriter struct {
	s  *Service
	id string
}

func (w stdioWriter) Write(ctx context.Context, frame []byte) error {
	s, id := w.s, w.id
	_, turn := claudestdio.UserFrameText(frame)
	settle := func(error) {}
	if turn {
		settle = s.beginTurnInput(ctx, id)
	}
	var err error
	if row, worker := s.workerSession(id); worker {
		err = s.sendWorkerCommand(ctx, row, "input", append(append([]byte(nil), frame...), '\n'))
	} else if turn {
		err = s.trackTurnSubmissionContext(ctx, id, func() error { return s.Manager.SendInput(id, frame) })
	} else {
		err = s.Manager.SendInput(id, frame)
	}
	settle(err)
	return err
}
