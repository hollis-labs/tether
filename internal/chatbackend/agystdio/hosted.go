package agystdio

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

// HostedConfig connects the internal shim-agy bridge. Send writes directly,
// without noting input to Tap a second time. Probe requires the versioned shim
// status protocol; it orders admission after boot and reattached work.
type HostedConfig struct {
	Tap            *claudestdio.Tap
	Send           func(context.Context, []byte) error
	Busy           func() bool
	InitialPending int
	Probe          bool
}

type workerStatus struct {
	RequestID string `json:"request_id"`
	Active    bool   `json:"active"`
	Queued    int    `json:"queued"`
	Protocol  int    `json:"protocol"`
}

// HostedExecutor correlates raw output and interrupts to a private client turn
// token. The shim retains its independently generated durable result UUID.
type HostedExecutor struct {
	cfg         HostedConfig
	mu          sync.Mutex
	pending     int
	id          string
	line        func([]byte)
	ended       chan struct{}
	probes      map[string]chan workerStatus
	unsubscribe func()
}

func NewHostedExecutor(cfg HostedConfig) *HostedExecutor {
	e := &HostedExecutor{cfg: cfg, pending: cfg.InitialPending, probes: map[string]chan workerStatus{}}
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
				Event    string       `json:"event"`
				Type     string       `json:"type"`
				TurnID   string       `json:"tether_turn_id"`
				Response workerStatus `json:"response"`
			}
			if json.Unmarshal(raw, &h) != nil {
				return
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if h.Type == "control_response" {
				if ch := e.probes[h.Response.RequestID]; ch != nil {
					select {
					case ch <- h.Response:
					default:
					}
				}
				return
			}
			if h.Event == "" {
				return
			}
			if h.Event != "result" && e.pending == 0 {
				e.pending = 1
			}
			own := e.line != nil && h.TurnID == e.id
			if own {
				e.line(raw)
			}
			if h.Event == "result" {
				if e.pending > 0 {
					e.pending--
				}
				if own {
					e.line = nil
					close(e.ended)
				}
			}
		},
	})
	return e
}
func (e *HostedExecutor) Close() error { e.unsubscribe(); return nil }
func controlFrame(id, kind, target string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": map[string]string{"subtype": kind, "target_turn_id": target}})
	return b
}
func (e *HostedExecutor) probe(ctx context.Context) error {
	id := uuid.NewString()
	ch := make(chan workerStatus, 1)
	e.mu.Lock()
	e.probes[id] = ch
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.probes, id); e.mu.Unlock() }()
	if err := e.cfg.Send(ctx, controlFrame(id, "status", "")); err != nil {
		return err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case status := <-ch:
		if status.Protocol != 1 {
			return errors.New("agystdio: unsupported shim control protocol")
		}
		if status.Active || status.Queued > 0 {
			return chatbackend.ErrBusy
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.cfg.Tap.Done():
		return chatbackend.ErrClosed
	case <-timer.C:
		return errors.New("agystdio: shim status unanswered")
	}
}
func (e *HostedExecutor) StartTurn(ctx context.Context, text string, line func([]byte)) (func() error, error) {
	var ended chan struct{}
	id := uuid.NewString()
	err := e.cfg.Tap.Input(nil, e, func() error {
		e.mu.Lock()
		busy := e.line != nil || e.pending > 0 || e.cfg.Busy()
		e.mu.Unlock()
		if busy {
			return chatbackend.ErrBusy
		}
		// The status response follows all prior worker output, including a boot or
		// reattached turn's result. Input's lock excludes other Service input writers.
		if e.cfg.Probe {
			if err := e.probe(ctx); err != nil {
				return err
			}
		}
		e.mu.Lock()
		if e.line != nil || e.pending > 0 {
			e.mu.Unlock()
			return chatbackend.ErrBusy
		}
		e.id = id
		e.line = line
		e.ended = make(chan struct{})
		ended = e.ended
		e.pending++
		e.mu.Unlock()
		var frame map[string]any
		_ = json.Unmarshal(claudestdio.UserFrame(text), &frame)
		frame["tether_turn_id"] = id
		data, _ := json.Marshal(frame)
		if err := e.cfg.Send(ctx, data); err != nil {
			e.mu.Lock()
			e.line = nil
			if e.pending > 0 {
				e.pending--
			}
			e.mu.Unlock()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		defer func() {
			e.mu.Lock()
			if e.id == id {
				e.line = nil
			}
			e.mu.Unlock()
		}()
		select {
		case <-ended:
			return nil
		case <-e.cfg.Tap.Done():
			return chatbackend.ErrClosed
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Even a late delivery cannot interrupt a successor: the worker compares
			// this token with its active token before canceling the actual subprocess.
			if err := e.cfg.Send(interruptCtx, controlFrame(uuid.NewString(), "interrupt", id)); err != nil {
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
