package app

import "github.com/hollis-labs/go-runtime-events/runtimeevents"

// TurnOutputState is a read-only snapshot seam for CancelTurnAndWait. Snapshot
// before cancellation, then wait from the caller goroutine, never the event
// reader. The stable submission ID exists before any runtime output; ACP binds
// its own reducer ID internally. Empty ID means no turn is in progress.
// Captured completion channels remain valid after another turn or session exit.
type TurnOutputState interface {
	CurrentTurn() (turnID string, done <-chan struct{})
	// Hold only across a fresh snapshot and runtime cancel; release before waiting.
	LockSubmission() (unlock func())
	TurnAccepted(turnID string) bool
	CompletedTurn(markerID string) (outputTurnID string, ok bool)
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
func (o *sessionTurnOutput) ensureTurn() bool {
	if o.turnID != "" {
		return false
	}
	o.turnID = runtimeevents.NewTurnID()
	o.turnDone = make(chan struct{})
	return true
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
func (o *sessionTurnOutput) completeTurn(id string) {
	if o.reducerTurnID == id && o.turnID != "" {
		if o.completed == nil {
			o.completed = make(map[string]string)
		}
		o.completed[o.turnID] = id
		o.completedOrder = append(o.completedOrder, o.turnID)
		if len(o.completedOrder) > 64 {
			delete(o.completed, o.completedOrder[0])
			o.completedOrder = o.completedOrder[1:]
		}
		o.finishedTurns = append(o.finishedTurns, id)
		if len(o.finishedTurns) > 64 {
			o.finishedTurns = o.finishedTurns[len(o.finishedTurns)-64:]
		}
		o.settleTurn()
	}
}

func (o *sessionTurnOutput) settleTurn() {
	if o.turnDone != nil {
		close(o.turnDone)
	}
	o.accepted = false
	o.turnID = ""
	o.reducerTurnID = ""
	o.turnDone = nil
}

// Publish a provisional marker before calling the runtime: it may synchronously
// emit its final output during submit. A successful return never resurrects it.
// Failed steering cannot settle an existing turn or a later turn's marker.
func (s *Service) trackTurnSubmission(id string, submit func() error) error {
	value, ok := s.turnOutputs.Load(id)
	if !ok {
		return submit()
	}
	output := value.(*sessionTurnOutput)
	unlock := output.LockSubmission()
	output.mu.Lock()
	created := output.ensureTurn()
	done := output.turnDone
	output.mu.Unlock()
	unlock()
	err := submit()
	output.mu.Lock()
	if output.turnDone == done {
		if err == nil {
			output.accepted = true
		} else if created {
			output.settleTurn()
		}
	}
	output.mu.Unlock()
	return err
}
