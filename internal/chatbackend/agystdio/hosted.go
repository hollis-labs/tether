package agystdio

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

// HostedConfig connects the internal shim-agy streaming bridge. Send writes
// directly to the session, without noting input to Tap a second time.
type HostedConfig struct {
	Tap       *claudestdio.Tap
	Send      func(context.Context, []byte) error
	Interrupt func(context.Context) error
	Busy      func() bool
}

// HostedExecutor follows only the submitted turn on the shim's raw output.
// The worker publishes its result only after the actual agy child exits.
type HostedExecutor struct {
	cfg         HostedConfig
	mu          sync.Mutex
	pending     int
	line        func([]byte)
	ended       chan struct{}
	unsubscribe func()
}

func NewHostedExecutor(cfg HostedConfig) *HostedExecutor {
	e := &HostedExecutor{cfg: cfg}
	e.unsubscribe = cfg.Tap.Subscribe(claudestdio.Subscriber{
		Input: func(claudestdio.InputNote) { e.mu.Lock(); e.pending++; e.mu.Unlock() },
		InputFailed: func(claudestdio.InputNote) {
			e.mu.Lock()
			if e.pending > 0 {
				e.pending--
			}
			e.mu.Unlock()
		},
		Line: func(raw []byte) {
			var h struct {
				Event string `json:"event"`
			}
			_ = json.Unmarshal(raw, &h)
			if h.Event == "" {
				return
			} // control responses are not model events
			e.mu.Lock()
			defer e.mu.Unlock()
			// Reattach can begin with a partial turn after its init was acknowledged.
			if h.Event != "result" && e.pending == 0 {
				e.pending = 1
			}
			if e.line != nil {
				e.line(raw)
			}
			if h.Event == "result" {
				if e.pending > 0 {
					e.pending--
				}
				if e.line != nil {
					e.line = nil
					close(e.ended)
				}
			}
		},
	})
	return e
}
func (e *HostedExecutor) Close() error { e.unsubscribe(); return nil }
func (e *HostedExecutor) StartTurn(ctx context.Context, text string, line func([]byte)) (func() error, error) {
	frame := claudestdio.UserFrame(text)
	var ended chan struct{}
	// Input serializes reservation with every ordinary Service input writer.
	err := e.cfg.Tap.Input(nil, e, func() error {
		e.mu.Lock()
		if e.line != nil || e.pending > 0 || e.cfg.Busy() {
			e.mu.Unlock()
			return chatbackend.ErrBusy
		}
		e.line = line
		e.ended = make(chan struct{})
		ended = e.ended
		e.pending++
		e.mu.Unlock()
		if err := e.cfg.Send(ctx, frame); err != nil {
			e.mu.Lock()
			e.line = nil
			e.pending--
			e.mu.Unlock()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		defer func() { e.mu.Lock(); e.line = nil; e.mu.Unlock() }()
		select {
		case <-ended:
			return nil
		case <-e.cfg.Tap.Done():
			return chatbackend.ErrClosed
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := e.cfg.Interrupt(interruptCtx); err != nil {
				return err
			}
			select {
			case <-ended:
				return ctx.Err()
			case <-e.cfg.Tap.Done():
				return chatbackend.ErrClosed
			case <-interruptCtx.Done():
				return interruptCtx.Err()
			}
		}
	}, nil
}
