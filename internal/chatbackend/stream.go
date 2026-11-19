package chatbackend

import (
	"iter"
	"sync"
	"sync/atomic"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
)

// Stream is a Run a backend fills as events arrive. Emit never blocks: a slow consumer must not stall the backend that
// reads the agent (events queue until the consumer takes them). It is safe for concurrent use.
type Stream struct {
	id       string
	cancel   func()
	mu       sync.Mutex
	queue    []chatstream.Event
	done     bool // the terminal event was emitted
	notify   chan struct{}
	consumed atomic.Bool
}

// NewStream returns an empty Stream for run id. cancel is what Cancel calls (once per call; the backend decides what a
// repeat means). It may be nil.
func NewStream(id string, cancel func()) *Stream {
	return &Stream{id: id, cancel: cancel, notify: make(chan struct{}, 1)}
}

// ID implements Run.
func (s *Stream) ID() string { return s.id }

// Cancel implements Run.
func (s *Stream) Cancel() {
	if s.cancel != nil && !s.Done() {
		s.cancel()
	}
}

// Done reports whether the terminal event was emitted.
func (s *Stream) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Emit appends events, stamping the run id. Events after the terminal one are dropped, so a run always ends after
// exactly one terminal event.
func (s *Stream) Emit(evs ...chatstream.Event) {
	s.mu.Lock()
	for _, ev := range evs {
		if s.done {
			break
		}
		ev.RunID = s.id
		s.queue = append(s.queue, ev)
		if ev.IsTerminal() {
			s.done = true
		}
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Events implements Run. The sequence ends after the terminal event; a consumer that stops early just stops reading.
func (s *Stream) Events() iter.Seq2[chatstream.Event, error] {
	return func(yield func(chatstream.Event, error) bool) {
		if !s.consumed.CompareAndSwap(false, true) {
			return
		}
		for {
			s.mu.Lock()
			batch := s.queue
			s.queue = nil
			s.mu.Unlock()
			for _, ev := range batch {
				if !yield(ev, nil) || ev.IsTerminal() {
					return
				}
			}
			if len(batch) == 0 {
				<-s.notify
			}
		}
	}
}
