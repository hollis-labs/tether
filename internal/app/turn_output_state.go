package app

import (
	"context"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// TurnCompletion retains the bound Output and whether process exit flushed it.
type TurnCompletion struct {
	OutputTurnID string
	OutputKind   turnoutput.Kind
	StopReason   string
	SessionEnded bool
	// Synthetic settles an accepted empty turn without publishing an Output.
	Synthetic bool
	// Superseded includes a successor that already completed before the query.
	Superseded bool
}

// TurnOutputState is a read-only snapshot seam for CancelTurnAndWait. Snapshot
// before cancellation, then wait from the caller goroutine, never the event
// reader. The stable submission ID exists before any runtime output; ACP binds
// its own reducer ID internally. Empty ID means no turn is in progress.
// Captured completion channels remain valid after another turn or session exit.
type TurnOutputState interface {
	CurrentTurn() (turnID string, done <-chan struct{})
	// Hold only across a fresh snapshot and runtime cancel; release before waiting.
	LockSubmission() (unlock func())
	LockSubmissionContext(context.Context) (unlock func(), err error)
	TurnAccepted(turnID string) bool
	CompletedTurn(markerID string) (outputTurnID string, ok bool)
	CompletedTurnDetails(markerID string) (TurnCompletion, bool)
}

func (s *Service) SessionTurnOutputState(sessionID string) (TurnOutputState, bool) {
	state, ok := s.turnOutputs.Load(sessionID)
	if !ok {
		return nil, false
	}
	return state.(*sessionTurnOutput), true
}

func (o *sessionTurnOutput) CurrentTurn() (string, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.turnID, o.turnDone
}

func (o *sessionTurnOutput) LockSubmission() func() {
	o.submissionGate.Lock()
	return o.submissionGate.Unlock
}

// LockSubmissionContext acquires the existing submission mutex without a
// waiter goroutine. Cancellation cannot leave a future acquisition behind.
func (o *sessionTurnOutput) LockSubmissionContext(ctx context.Context) (func(), error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if o.submissionGate.TryLock() {
			return o.submissionGate.Unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (o *sessionTurnOutput) CompletedTurnDetails(markerID string) (TurnCompletion, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	completion, ok := o.completedDetails[markerID]
	if ok {
		completion.Superseded = (o.turnID != "" && o.turnID != markerID) || (len(o.completedOrder) > 0 && o.completedOrder[len(o.completedOrder)-1] != markerID)
	}
	return completion, ok
}

func (o *sessionTurnOutput) TurnAccepted(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return id != "" && o.turnID == id && o.accepted
}

// CompletedTurn verifies the stable marker was bound to this reducer Output.
// No record exists for a rejected submission or process exit before any Output.
func (o *sessionTurnOutput) CompletedTurn(markerID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	id, ok := o.completed[markerID]
	return id, ok
}

// ensureTurn is called with mu held. Submission and steering share one marker.
func (o *sessionTurnOutput) ensureTurn() {
	if o.turnID != "" {
		return
	}
	o.turnID = runtimeevents.NewTurnID()
	o.turnDone = make(chan struct{})
}

// bindTurn associates a raw runtime turn with a stable submission snapshot.
// Called with mu held; native providers bind through the NewTurnID hook.
func (o *sessionTurnOutput) bindTurn(id string) {
	if o.reducerTurnID != "" && o.reducerTurnID != id {
		o.settleTurn()
	}
	o.ensureTurn()
	o.reducerTurnID = id
	o.accepted = true
}

// completeTurn follows Output processing, including an empty final, before the
// synchronous reader callback returns. Failed submissions use settleTurn.
func (o *sessionTurnOutput) completeTurn(output turnoutput.Output, sessionEnded bool) {
	id := output.TurnID
	// Reducer completion is authoritative even when out-of-order output does
	// not complete our current marker. Late frames for that turn stay closed.
	o.finishedTurns = append(o.finishedTurns, id)
	if len(o.finishedTurns) > 64 {
		o.finishedTurns = o.finishedTurns[len(o.finishedTurns)-64:]
	}
	if o.reducerTurnID == id && o.turnID != "" {
		if o.completed == nil {
			o.completed = make(map[string]string)
		}
		o.completed[o.turnID] = id
		if o.completedDetails == nil {
			o.completedDetails = make(map[string]TurnCompletion)
		}
		o.completedDetails[o.turnID] = TurnCompletion{OutputTurnID: id, OutputKind: output.Kind, StopReason: output.StopReason, SessionEnded: sessionEnded}
		o.completedOrder = append(o.completedOrder, o.turnID)
		if len(o.completedOrder) > 64 {
			delete(o.completed, o.completedOrder[0])
			delete(o.completedDetails, o.completedOrder[0])
			o.completedOrder = o.completedOrder[1:]
		}
		o.settleTurn()
	}
}

func (o *sessionTurnOutput) settleTurn() {
	if o.turnDone != nil {
		close(o.turnDone)
	}
	o.accepted = false
	o.unboundTerminal = nil
	o.unboundAmbiguous = false
	o.submissions = 0
	o.turnID = ""
	o.reducerTurnID = ""
	o.turnDone = nil
}

// Publish a provisional marker before calling the runtime: it may synchronously
// emit its final output during submit. A successful return never resurrects it.
// Failed steering cannot settle an existing turn or a later turn's marker.
func (s *Service) trackTurnSubmission(id string, submit func() error) error {
	return s.trackTurnSubmissionContext(context.Background(), id, submit)
}

// Caller cancellation bounds gate acquisition without holding it across runtime
// entry. The legacy helper remains available to input paths without a context.
func (s *Service) trackTurnSubmissionContext(ctx context.Context, id string, submit func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, ok := s.turnOutputs.Load(id)
	if !ok {
		return submit()
	}
	output := value.(*sessionTurnOutput)
	unlock, err := output.LockSubmissionContext(ctx)
	if err != nil {
		return err
	}
	output.mu.Lock()
	output.ensureTurn()
	// An ID-less terminal retained during another pending submission cannot
	// establish which concurrent submission ran. Do not lend it to steering.
	if output.submissions > 0 {
		output.unboundTerminal = nil
		output.unboundAmbiguous = true
	}
	output.submissions++
	done := output.turnDone
	output.mu.Unlock()
	unlock()
	err = ctx.Err()
	if err == nil {
		err = submit()
	}
	output.mu.Lock()
	if output.turnDone == done {
		output.submissions--
		if err == nil {
			output.accepted = true
			if output.unboundTerminal != nil {
				output.completeEmptyTerminal(output.unboundTerminal.kind, output.unboundTerminal.stopReason)
			}
		} else {
			output.unboundTerminal = nil
			if output.submissions == 0 && !output.accepted {
				output.settleTurn()
			}
		}
	}
	output.mu.Unlock()
	return err
}

// A terminal may arrive synchronously before submit returns. Retain it on that
// provisional marker, but only record a synthetic completion after acceptance.
// Called with mu held; a bound turn always completes through the reducer.
type emptyTurnTerminal struct {
	kind       turnoutput.Kind
	stopReason string
}

// Provider terminals without IDs have no host begin boundary. A duplicate
// after acceptance of a new submission is indistinguishable from its empty
// response. Preserve empty-turn settlement; resolving that ambiguity requires
// an upstream explicit begin/turn ID, rather than guessing from timing.
func (o *sessionTurnOutput) emptyTerminal(kind turnoutput.Kind, stopReason string) {
	if o.turnID == "" || o.reducerTurnID != "" || o.unboundAmbiguous {
		return
	}
	o.unboundTerminal = &emptyTurnTerminal{kind: kind, stopReason: stopReason}
	if o.accepted {
		o.completeEmptyTerminal(kind, stopReason)
	}
}

func (o *sessionTurnOutput) completeEmptyTerminal(kind turnoutput.Kind, stopReason string) {
	if !o.accepted || o.turnID == "" || o.reducerTurnID != "" {
		return
	}
	id := o.turnID
	o.reducerTurnID = id
	o.completeTurn(turnoutput.Output{TurnID: id, Kind: kind, StopReason: stopReason}, false)
	completion := o.completedDetails[id]
	completion.Synthetic = true
	o.completedDetails[id] = completion
}
