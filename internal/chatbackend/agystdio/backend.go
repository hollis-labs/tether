// Package agystdio runs chat turns through agy's existing per-turn subprocess runtime.
// The runtime owns argv, sandboxing, permissions and --conversation continuity.
// Observed is empty: this backend captures only turns it submits, not other seat inputs.
package agystdio

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream/adapter/agy"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

// Executor reserves the session synchronously, then returns the function that runs
// its subprocess. Lines are raw agy NDJSON. The reservation is released when wait returns.
// Cancellation must stop and reap the subprocess before wait returns.
type Executor interface {
	StartTurn(context.Context, string, func([]byte)) (wait func() error, err error)
}

// Config supplies the seat's executor and optional model metadata.
type Config struct {
	Executor Executor
	Model    string
}

// Backend implements chatbackend.Backend and the chatwire backend lifecycle.
type Backend struct {
	cfg      Config
	mu       sync.Mutex
	closed   bool
	active   bool
	cancel   context.CancelFunc
	done     chan struct{}
	observed chan chatbackend.ObservedRun
}

// New creates a backend. Run closes it when the serving context ends.
func New(cfg Config) *Backend {
	return &Backend{cfg: cfg, observed: make(chan chatbackend.ObservedRun)}
}
func (b *Backend) Info() chatbackend.Info {
	return chatbackend.Info{Deltas: agy.New().Capabilities().Text == chatstream.GranularityToken}
}
func (b *Backend) Observed() <-chan chatbackend.ObservedRun { return b.observed }
func (b *Backend) Run(ctx context.Context) error {
	<-ctx.Done()
	b.mu.Lock()
	b.closed = true
	if b.cancel != nil {
		b.cancel()
	}
	done := b.done
	b.mu.Unlock()
	if done != nil {
		<-done
	}
	// Run is called once per backend by the runner.
	close(b.observed)
	return ctx.Err()
}

func (b *Backend) Submit(ctx context.Context, t chatbackend.Turn) (chatbackend.Run, error) {
	if t.Text == "" {
		return nil, errors.New("agystdio: empty turn")
	}
	input, err := t.InputText()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, chatbackend.ErrClosed
	}
	if b.active {
		return nil, chatbackend.ErrBusy
	}
	if b.cfg.Executor == nil {
		return nil, errors.New("agystdio: no executor")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := t.RunID
	if id == "" {
		id = "run_" + uuid.NewString()
	}
	runCtx, cancel := context.WithCancel(ctx)
	stream := chatbackend.NewStream(id, cancel)
	dec := agy.New().NewDecoder(chatstream.DecodeOptions{RunID: id, Provider: "antigravity", Model: b.cfg.Model})
	// The runtime invokes line synchronously on its reader, and wait joins it.
	line := func(raw []byte) {
		evs, e := dec.Decode(chatstream.Frame{Data: raw})
		if e == nil {
			stream.Emit(evs...)
		}
	}
	wait, err := b.cfg.Executor.StartTurn(runCtx, input, line)
	if err != nil {
		cancel()
		return nil, err
	}
	b.active, b.cancel, b.done = true, cancel, make(chan struct{})
	done := b.done
	go func() {
		err := wait()
		// A cancel always closes open parts through the decoder and yields abort,
		// including when the CLI exits without writing an interrupted result.
		if runCtx.Err() != nil && !stream.Done() {
			for _, ev := range dec.Close(runCtx.Err()) {
				if ev.IsTerminal() {
					ev.Verb = chatstream.VerbRunAbort
					ev.Code = ""
					ev.Message = ""
					ev.Reason = "interrupted"
				}
				stream.Emit(ev)
			}
		} else {
			stream.Emit(dec.Close(err)...)
		}
		cancel()
		b.mu.Lock()
		b.active = false
		b.cancel = nil
		close(done)
		b.mu.Unlock()
	}()
	return stream, nil
}

var _ chatbackend.Backend = (*Backend)(nil)
