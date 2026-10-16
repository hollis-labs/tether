package events

import (
	"context"
	"fmt"
	"log"
	"slices"
)

// Stage fills an event's envelope from verified context just before the event
// is written (the stamp stage of the event stream contract, X4). It is the one
// place a producer's scope, correlation and actor are decided, so a value is
// either verified here or left empty; a stage never copies a value out of a
// payload, a request argument or a message body, and never guesses.
//
// Stamp runs before the bus lock and may read ctx (callcontext). It must only
// fill what it can verify and leave a field a producer already set alone. A
// stage that returns an error or panics never loses the event: the bus reports
// it through BusOptions.OnStageError and publishes what it has.
type Stage interface {
	Stamp(ctx context.Context, e *Event) error
}

// StageFunc adapts a function to Stage.
type StageFunc func(ctx context.Context, e *Event) error

// Stamp implements Stage.
func (f StageFunc) Stamp(ctx context.Context, e *Event) error { return f(ctx, e) }

// TxSafe is implemented by stages that never touch the database. PublishTx
// runs inside a caller's transaction that holds the only connection, so it
// runs only the stages that report true; a stage that may read the database
// would wait for the connection its caller holds.
type TxSafe interface{ TxSafe() bool }

// StageError names the stage that failed and the event it was stamping.
type StageError struct {
	Stage int
	Kind  string
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("events: stamp stage %d on %q: %v", e.Stage, e.Kind, e.Err)
}
func (e *StageError) Unwrap() error { return e.Err }

// stamp runs the stages in order. tx restricts it to TxSafe stages. A failing
// stage is reported and skipped; its partial work on a copy is discarded so a
// half-stamped envelope never persists.
func (b *memBus) stamp(ctx context.Context, e Event, tx bool) Event {
	for i, s := range b.stages {
		if tx {
			if safe, ok := s.(TxSafe); !ok || !safe.TxSafe() {
				continue
			}
		}
		next := cloneEvent(e)
		if err := runStage(ctx, s, &next); err != nil {
			b.onStageError(&StageError{Stage: i, Kind: e.Kind, Err: err})
			continue
		}
		e = next
	}
	return e
}

// cloneEvent copies e deeply enough that a stage can write anywhere in it
// without touching the original. Every Envelope field is a value except
// Class.Labels; add any new slice or map field here.
func cloneEvent(e Event) Event {
	e.Envelope.Class.Labels = slices.Clone(e.Envelope.Class.Labels)
	return e
}

func runStage(ctx context.Context, s Stage, e *Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.Stamp(ctx, e)
}

func defaultOnStageError(err *StageError) { log.Printf("%v", err) }
