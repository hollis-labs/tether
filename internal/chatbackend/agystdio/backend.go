// Package agystdio runs chat turns through agy's existing per-turn subprocess runtime.
// The runtime owns argv, sandboxing, permissions and --conversation continuity.
// Observed is empty: this backend captures only turns it submits, not other seat inputs.
package agystdio

import (
	"context"
	"errors"
	"strings"
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
	// Keep the terminal until wait verifies actual exit and native continuity:
	// agy may print SUCCESS before a nonzero exit or a replaced conversation is detected.
	block := ""
	if len(t.Context) > 0 {
		block = strings.TrimSuffix(input[:len(input)-len(t.Text)], "\n")
	}
	var terminal *chatstream.Event
	line := func(raw []byte) {
		raw = outputFrame(raw, block)
		if len(raw) == 0 {
			return
		}
		evs, e := dec.Decode(chatstream.Frame{Data: raw})
		if e == nil {
			for _, ev := range evs {
				// Opaque provider diagnostics can echo structured input as well as text.
				// They are not conversation content when a private context was supplied.
				if len(t.Context) > 0 && (ev.Raw != nil || (ev.Verb == chatstream.VerbActivity && ev.Kind != "agy.permission_denied")) {
					continue
				}
				if ev.IsTerminal() {
					saved := ev
					terminal = &saved
				} else {
					stream.Emit(ev)
				}
			}
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
		// Release the active slot under the same lock that publishes the terminal,
		// so a consumer can immediately submit the next turn.
		b.mu.Lock()
		// A cancel closes open parts and aborts, including when the CLI exits
		// without an interrupted result. Runtime failures cannot become success.
		if terminal != nil {
			ev := *terminal
			converted := runCtx.Err() != nil || (err != nil && ev.Verb == chatstream.VerbRunFinish)
			switch {
			case runCtx.Err() != nil:
				ev.Verb = chatstream.VerbRunAbort
				ev.Code = ""
				ev.Message = ""
				ev.Reason = "interrupted"
			case err != nil && ev.Verb == chatstream.VerbRunFinish:
				ev.Verb = chatstream.VerbRunError
				ev.Code = "process_error"
				ev.Message = "agy process failed or lost its native conversation"
				ev.Reason = ""
			}
			if converted {
				if ev.Usage != nil {
					stream.Emit(chatstream.Event{V: chatstream.SchemaVersion, Verb: chatstream.VerbUsage, Time: ev.Time, Usage: ev.Usage})
				}
				ev.RawReason = ""
				ev.Usage = nil
			}
			stream.Emit(ev)
		} else {
			cause := err
			if cause != nil {
				cause = errors.New("agy process failed")
			}
			if runCtx.Err() != nil {
				cause = runCtx.Err()
			}
			for _, ev := range dec.Close(cause) {
				if runCtx.Err() != nil && ev.IsTerminal() {
					ev.Verb = chatstream.VerbRunAbort
					ev.Code = ""
					ev.Message = ""
					ev.Reason = "interrupted"
				}
				stream.Emit(ev)
			}
		}

		cancel()
		b.active = false
		b.cancel = nil
		close(done)
		b.mu.Unlock()
	}()
	return stream, nil
}

var _ chatbackend.Backend = (*Backend)(nil)
