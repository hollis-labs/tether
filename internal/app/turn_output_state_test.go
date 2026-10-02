package app

import (
	"errors"
	"testing"

	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestTurnOutputStateSnapshotsCompleteBeforeCallbackReturns(t *testing.T) {
	for _, feed := range []string{"native", "ACP", "empty final", "session exit"} {
		t.Run(feed, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			state, ok := svc.SessionTurnOutputState("s1")
			if !ok {
				t.Fatal("state lookup missing")
			}
			if id, _ := state.CurrentTurn(); id != "" {
				t.Fatal("idle reducer has open turn")
			}
			switch feed {
			case "ACP":
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "ACP-turn"})
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta, TurnID: "ACP-turn", Payload: []byte(`{"content":"reply"}`)})
			case "empty final":
				output.observeProvider(gopevents.Thinking{Text: "not public"})
			default:
				output.observeProvider(gopevents.Delta{Text: "reply"})
			}
			id, done := state.CurrentTurn()
			if id == "" || done == nil {
				t.Fatal("open reducer turn has no snapshot")
			}
			select {
			case <-done:
				t.Fatal("open turn already completed")
			default:
			}
			switch feed {
			case "ACP":
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "ACP-turn"})
			case "session exit":
				output.flush()
			default:
				output.observeProvider(gopevents.Done{})
			}
			select {
			case <-done:
			default:
				t.Fatal("callback returned before completion signaled")
			}
			if current, _ := state.CurrentTurn(); current != "" {
				t.Fatal("completed turn still open")
			}
			if feed != "empty final" {
				outputs := outputEvents(t, svc)
				expectedID := id
				if feed == "ACP" {
					expectedID = "ACP-turn"
				}
				if len(outputs) != 1 || outputs[0].TurnID != expectedID {
					t.Fatalf("snapshot ID != emitted ID: %s %+v", id, outputs)
				}
			}
			output.observeProvider(gopevents.Delta{Text: "next reply"})
			next, nextDone := state.CurrentTurn()
			if next == id || nextDone == nil {
				t.Fatal("next turn reused completion state")
			}
			select {
			case <-done:
			default:
				t.Fatal("old snapshot was invalidated")
			}
			select {
			case <-nextDone:
				t.Fatal("next turn already ended")
			default:
			}
		})
	}
}

func TestTurnSubmissionTracksBeforeOutputAndPreservesSteering(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	var id string
	var done <-chan struct{}
	if err := svc.trackTurnSubmission("s1", func() error {
		id, done = output.CurrentTurn()
		if id == "" || done == nil {
			t.Fatal("submission has no marker before output")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("steering failed")
	if err := svc.trackTurnSubmission("s1", func() error {
		current, ch := output.CurrentTurn()
		if current != id || ch != done {
			t.Fatal("steering replaced marker")
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("failed steering completed running turn")
	default:
	}
	output.observeProvider(gopevents.Delta{Text: "reply"})
	output.observeProvider(gopevents.Done{Text: "reply"})
	select {
	case <-done:
	default:
		t.Fatal("output did not complete marker")
	}
	if got := outputEvents(t, svc); len(got) != 1 || got[0].TurnID != id {
		t.Fatalf("marker not bound: %+v", got)
	}
}

func TestTurnSubmissionSynchronousCompletionAndFailure(t *testing.T) {
	for _, mode := range []string{"synchronous", "failure", "exit", "ACP"} {
		t.Run(mode, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			var done <-chan struct{}
			err := svc.trackTurnSubmission("s1", func() error {
				id, ch := output.CurrentTurn()
				done = ch
				if id == "" {
					t.Fatal("no provisional marker")
				}
				switch mode {
				case "synchronous":
					output.observeProvider(gopevents.Done{Text: "reply"})
				case "failure":
					return errors.New("rejected")
				case "exit":
					output.flush()
				case "ACP":
					output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "runtime-id", Payload: []byte(`{"text":"reply"}`)})
				}
				return nil
			})
			if mode == "failure" && err == nil {
				t.Fatal("failed submission swallowed")
			}
			select {
			case <-done:
			default:
				t.Fatal("marker was not settled")
			}
			if id, _ := output.CurrentTurn(); id != "" {
				t.Fatal("return resurrected completed marker")
			}
			if mode == "ACP" {
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "runtime-id"})
				if id, _ := output.CurrentTurn(); id != "" {
					t.Fatal("duplicate ACP event reopened marker")
				}
			}
		})
	}
}

func TestFailedSubmissionCannotSettleLaterTurn(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	var laterID string
	var laterDone <-chan struct{}
	_ = svc.trackTurnSubmission("s1", func() error {
		output.observeProvider(gopevents.Done{Text: "first"})
		if err := svc.trackTurnSubmission("s1", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		laterID, laterDone = output.CurrentTurn()
		return errors.New("late failure")
	})
	if id, _ := output.CurrentTurn(); id != laterID || id == "" {
		t.Fatal("late failure cleared newer marker")
	}
	select {
	case <-laterDone:
		t.Fatal("late failure completed newer marker")
	default:
	}
}

func TestCurrentTurnVisibleDuringSubmission(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	submitting := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- svc.trackTurnSubmission("s1", func() error {
			close(submitting)
			<-release
			return nil
		})
	}()
	<-submitting
	id, done := output.CurrentTurn()
	if id == "" || done == nil {
		close(release)
		<-returned
		t.Fatal("pending submit was invisible")
	}
	output.observeProvider(gopevents.Done{Text: "synchronous completion"})
	select {
	case <-done:
	default:
		close(release)
		<-returned
		t.Fatal("pending submit missed completion")
	}
	close(release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatal("successful return resurrected marker")
	}
}

func TestSubmissionGateProtectsMarkerInstallationWithoutBlockingRuntime(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	unlock := output.LockSubmission()
	attempting := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		close(attempting)
		returned <- svc.trackTurnSubmission("s1", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-attempting
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatal("marker installed while gate held")
	}
	select {
	case <-entered:
		t.Fatal("runtime entered while gate held")
	default:
	}
	unlock()
	<-entered
	// SendInput is still blocked, but cancellation can acquire the gate.
	unlock = output.LockSubmission()
	id, done := output.CurrentTurn()
	if id == "" || output.TurnAccepted(id) {
		t.Fatal("pending marker incorrectly accepted")
	}
	output.observeProvider(gopevents.ToolUse{ID: "tool", Name: "shell"})
	if !output.TurnAccepted(id) {
		t.Fatal("first reduced event did not accept turn")
	}
	unlock()
	close(release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{Text: "reply"})
	<-done
	if completed, ok := output.CompletedTurn(id); !ok || completed != id {
		t.Fatal("completion does not match marker")
	}
}

func TestTurnAcceptedOnReturnAndBindingAwareCompletion(t *testing.T) {
	for _, mode := range []string{"ACP", "failure", "exit"} {
		t.Run(mode, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			var id string
			_ = svc.trackTurnSubmission("s1", func() error {
				id, _ = output.CurrentTurn()
				if output.TurnAccepted(id) {
					t.Fatal("provisional marker accepted")
				}
				if mode == "failure" {
					return errors.New("not accepted")
				}
				return nil
			})
			if mode == "failure" {
				if _, ok := output.CompletedTurn(id); ok {
					t.Fatal("failed submission claims Output")
				}
				return
			}
			if !output.TurnAccepted(id) {
				t.Fatal("successful submission not accepted")
			}
			if mode == "exit" {
				output.flush()
				if _, ok := output.CompletedTurn(id); ok {
					t.Fatal("exit without Output claims completion")
				}
				return
			}
			output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "ACP-id"})
			if current, _ := output.CurrentTurn(); current != id {
				t.Fatal("binding replaced stable marker")
			}
			output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "ACP-id"})
			if got, ok := output.CompletedTurn(id); !ok || got != "ACP-id" {
				t.Fatal("ACP binding not recorded")
			}
		})
	}
}
