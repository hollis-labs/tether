package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/events"
)

func interruptTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type auditFailurePublisher struct {
	events.Bus
	fail error
}

func (b auditFailurePublisher) Publish(ctx context.Context, event events.Event) error {
	if event.Kind == events.KindSessionTurnInterruptCompleted {
		return b.fail
	}
	return b.Bus.Publish(ctx, event)
}

func TestCancelTurnAuditEarlyReturnsAndOutcomeFailure(t *testing.T) {
	for _, kind := range []string{"actor", "missing", "unsupported", "outcome failure"} {
		t.Run(kind, func(t *testing.T) {
			var output *sessionTurnOutput
			svc, state, _ := interruptHarness(t, func(context.Context) error { output.observeProvider(gopevents.Done{}); return nil })
			output = state
			output.observeProvider(gopevents.Delta{Text: "working"})
			sessionID, actor := "s1", "actor"
			outcome := "error"
			var want error
			switch kind {
			case "actor":
				actor = ""
			case "missing":
				sessionID = "absent"
				want = agentsessions.ErrSessionNotRunning
			case "unsupported":
				svc2, state2, _ := interruptHarness(t, nil)
				svc, output = svc2, state2
				output.observeProvider(gopevents.Delta{Text: "working"})
				outcome = string(TurnInterruptUnsupported)
				want = agentsessions.ErrInterruptUnsupported
			case "outcome failure":
				want = errors.New("audit unavailable")
				svc.Bus = auditFailurePublisher{Bus: svc.Bus, fail: want}
			}
			_, callErr := svc.CancelTurnAndWait(interruptTestContext(t), sessionID, actor)
			err := callErr
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("error=%v, want %v", err, want)
			}
			if kind == "outcome failure" {
				return
			}
			rows, err := svc.Store.ListEventsBySession(sessionID, 20, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.Kind != events.KindSessionTurnInterruptCompleted {
					continue
				}
				var audit events.TurnInterruptEvent
				if err := json.Unmarshal([]byte(row.PayloadJSON), &audit); err != nil {
					t.Fatal(err)
				}
				if audit.Actor != actor || audit.SessionID != sessionID || audit.Result != outcome || audit.Error != callErr.Error() {
					t.Fatalf("audit=%+v error=%v", audit, callErr)
				}
				found = true
			}
			if !found {
				t.Fatal("outcome audit missing")
			}
		})
	}
}

func TestCancelTurnExitFlushIsSessionEnded(t *testing.T) {
	var output *sessionTurnOutput
	svc, state, _ := interruptHarness(t, func(context.Context) error { output.flush(); return nil })
	output = state
	output.observeProvider(gopevents.Delta{Text: "partial"})
	result, err := svc.CancelTurnAndWait(interruptTestContext(t), "s1", "actor")
	requireRefusal(t, err, TurnInterruptSessionEnded)
	if !errors.Is(err, agentsessions.ErrSessionNotRunning) || result.OutputKind == "" || result.StopReason != "error" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCancelTurnRejectsEventDrivenSuccessor(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "already ended"}[ended], func(t *testing.T) {
			var output *sessionTurnOutput
			svc, state, _ := interruptHarness(t, func(context.Context) error {
				output.observeProvider(gopevents.Done{})
				output.observeProvider(gopevents.Delta{Text: "successor"})
				if ended {
					output.observeProvider(gopevents.Done{})
				}
				return nil
			})
			output = state
			output.observeProvider(gopevents.Delta{Text: "first"})
			_, err := svc.CancelTurnAndWait(interruptTestContext(t), "s1", "actor")
			requireRefusal(t, err, TurnInterruptSuperseded)
		})
	}
}

func TestCancelTurnHoldsGateAndBoundsRuntimeAndCompletion(t *testing.T) {
	for _, block := range []string{"runtime", "terminal"} {
		t.Run(block, func(t *testing.T) {
			var output *sessionTurnOutput
			svc, state, _ := interruptHarness(t, func(ctx context.Context) error {
				if output.submissionGate.TryLock() {
					output.submissionGate.Unlock()
					t.Error("gate released before runtime cancel")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("runtime cancel has no deadline")
				}
				if block == "runtime" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			})
			output = state
			svc.InterruptCancelTimeout = 20 * time.Millisecond
			svc.InterruptDoneTimeout = 20 * time.Millisecond
			output.observeProvider(gopevents.Delta{Text: "working"})
			_, err := svc.CancelTurnAndWait(interruptTestContext(t), "s1", "actor")
			requireRefusal(t, err, TurnInterruptTimeout)
			if !output.submissionGate.TryLock() {
				t.Fatal("timeout retained gate")
			}
			output.submissionGate.Unlock()
		})
	}
}

type enteringInterruptState struct {
	TurnOutputState
	entered chan struct{}
}

func (s enteringInterruptState) LockSubmissionContext(ctx context.Context) (func(), error) {
	close(s.entered)
	return s.TurnOutputState.LockSubmissionContext(ctx)
}

func TestCancelTurnGateAcquisitionHonorsContext(t *testing.T) {
	calls := 0
	svc, output, _ := interruptHarness(t, func(context.Context) error { calls++; return nil })
	output.observeProvider(gopevents.Delta{Text: "working"})
	id, _ := output.CurrentTurn()
	unlock := output.LockSubmission()
	defer unlock()
	ctx, cancel := context.WithCancel(interruptTestContext(t))
	defer cancel()
	entered := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		_, err := svc.cancelTurnAndWait(ctx, "s1", "actor", enteringInterruptState{TurnOutputState: output, entered: entered}, id)
		returned <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	guard := interruptTestContext(t)
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("error=%v calls=%d", err, calls)
		}
	case <-guard.Done():
		t.Fatal("gate acquisition ignored context")
	}
}

func TestCancelTurnNonCooperativeRuntimeReleasesGate(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	svc, output, session := interruptHarness(t, func(context.Context) error {
		close(entered)
		<-release // Deliberately ignores its context, like a wedged stdin write.
		close(finished)
		return nil
	})
	svc.InterruptCancelTimeout = 20 * time.Millisecond
	output.observeProvider(gopevents.Delta{Text: "working"})
	id, _ := output.CurrentTurn()
	_, err := svc.CancelTurnAndWait(interruptTestContext(t), "s1", "actor")
	requireRefusal(t, err, TurnInterruptTimeout)
	select {
	case <-entered:
	default:
		t.Fatal("runtime cancel never entered")
	}
	if !output.submissionGate.TryLock() {
		t.Fatal("unreturned cancel retained gate")
	}
	output.submissionGate.Unlock()
	submitted := make(chan struct{})
	session.input = func(context.Context, []byte) error { close(submitted); return nil }
	returned := make(chan error, 1)
	go func() { returned <- svc.SendInput("s1", []byte("steer")) }()
	ctx := interruptTestContext(t)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("submission wedged behind unreturned cancel")
	}
	select {
	case <-submitted:
	default:
		t.Fatal("following input never reached runtime")
	}
	if current, _ := output.CurrentTurn(); current != id {
		t.Fatal("timeout altered marker")
	}
	select {
	case <-finished:
		t.Fatal("fake unexpectedly returned before release")
	default:
	}
}
