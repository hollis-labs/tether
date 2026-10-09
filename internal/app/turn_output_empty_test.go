package app

import (
	"context"
	"errors"
	"testing"

	gopevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	"github.com/hollis-labs/substrate/harness/adapters/providertest"
	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"
)

func TestTwoAcceptedEmptyNativeTurnsClearMarkers(t *testing.T) {
	run := providertest.Script(providertest.Exit(0))
	run.Repeat = true
	svc, id := nativeOutputLaunch(t, runtimes.Codex, "subprocess", run)
	for turn := 1; turn <= 2; turn++ {
		if err := svc.SendTurn(context.Background(), id, "empty response"); err != nil {
			t.Fatal(err)
		}
		state, ok := svc.SessionTurnOutputState(id)
		if !ok {
			t.Fatal("no state")
		}
		if marker, _ := state.CurrentTurn(); marker != "" {
			t.Fatalf("empty turn %d left marker %q", turn, marker)
		}
	}
	if outputs := outputEvents(t, svc); len(outputs) != 0 {
		t.Fatalf("empty turns published: %+v", outputs)
	}
}

func TestEmptyTerminalCompletionAtHostBoundary(t *testing.T) {
	for _, feed := range []string{"provider", "runtime"} {
		for _, synchronous := range []bool{false, true} {
			t.Run(feed+map[bool]string{false: " accepted", true: " provisional"}[synchronous], func(t *testing.T) {
				svc, output := outputHarness(t, nil)
				svc.turnOutputs.Store("s1", output)
				terminal := func() {
					if feed == "provider" {
						output.observeProvider(gopevents.Done{StopReason: "end_turn"})
					} else {
						output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, Payload: []byte(`{"stop_reason":"end_turn"}`)})
					}
				}
				// Settle the reducer so an identical terminal is suppressed on the next turn.
				terminal()
				var marker string
				var done <-chan struct{}
				err := svc.trackTurnSubmission("s1", func() error {
					marker, done = output.CurrentTurn()
					if synchronous {
						terminal()
						if current, _ := output.CurrentTurn(); current != marker {
							t.Fatal("provisional terminal settled before acceptance")
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !synchronous {
					terminal()
				}
				select {
				case <-done:
				default:
					t.Fatal("empty terminal did not close completion")
				}
				if current, _ := output.CurrentTurn(); current != "" {
					t.Fatalf("retained %q", current)
				}
				completion, ok := output.CompletedTurnDetails(marker)
				if !ok || !completion.Synthetic || completion.OutputTurnID != marker || completion.OutputKind != turnoutput.KindFinal || completion.StopReason != "end_turn" || completion.SessionEnded {
					t.Fatalf("completion: %+v %v", completion, ok)
				}
				if outputs := outputEvents(t, svc); len(outputs) != 0 {
					t.Fatalf("published empty output: %+v", outputs)
				}
				// A real answer still binds, publishes, and records its real completion.
				if err := svc.trackTurnSubmission("s1", func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				realID, _ := output.CurrentTurn()
				if feed == "provider" {
					output.observeProvider(gopevents.Done{Text: "real answer"})
				} else {
					output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta, TurnID: "real-runtime", Payload: []byte(`{"text":"real answer"}`)})
					output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "real-runtime"})
				}
				answerCompletion, ok := output.CompletedTurnDetails(realID)
				if !ok || answerCompletion.Synthetic {
					t.Fatalf("real turn changed: %+v %v", answerCompletion, ok)
				}
				outputs := outputEvents(t, svc)
				if len(outputs) != 1 || outputs[0].Kind != turnoutput.KindFinal || outputs[0].Text != "real answer" {
					t.Fatalf("real answer lost: %+v", outputs)
				}
			})
		}
	}
}

func TestRejectedEmptySubmissionDoesNotRecordCompletion(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	output.observeProvider(gopevents.Done{})
	var marker string
	err := svc.trackTurnSubmission("s1", func() error {
		marker, _ = output.CurrentTurn()
		output.observeProvider(gopevents.Done{})
		return errors.New("submission rejected")
	})
	if err == nil {
		t.Fatal("submission succeeded")
	}
	if current, _ := output.CurrentTurn(); current != "" {
		t.Fatal("failed marker retained")
	}
	if completion, ok := output.CompletedTurnDetails(marker); ok {
		t.Fatalf("failed submit recorded %+v", completion)
	}
}

func TestAmbiguousProvisionalTerminalIsNotLentToSteering(t *testing.T) {
	for _, phase := range []string{"before overlap", "during overlap", "after overlap"} {
		t.Run(phase, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			output.observeProvider(gopevents.Done{})
			var marker string
			var done <-chan struct{}
			err := svc.trackTurnSubmission("s1", func() error {
				marker, done = output.CurrentTurn()
				if phase == "before overlap" {
					output.observeProvider(gopevents.Done{})
				}
				if err := svc.trackTurnSubmission("s1", func() error {
					if phase == "during overlap" {
						output.observeProvider(gopevents.Done{})
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if phase == "after overlap" {
					output.observeProvider(gopevents.Done{})
				}
				return errors.New("creator rejected")
			})
			if err == nil {
				t.Fatal("creator failure hidden")
			}
			if current, _ := output.CurrentTurn(); current != marker || !output.TurnAccepted(marker) {
				t.Fatal("stale terminal completed steering")
			}
			select {
			case <-done:
				t.Fatal("ambiguous terminal closed marker")
			default:
			}
			output.observeProvider(gopevents.Done{Text: "steering answer"})
			if completion, ok := output.CompletedTurnDetails(marker); !ok || completion.Synthetic {
				t.Fatalf("real completion lost: %+v %v", completion, ok)
			}
		})
	}
}

func TestSuppressedFailureTerminalRetainsFailureCompletion(t *testing.T) {
	for _, feed := range []string{"provider", "runtime"} {
		t.Run(feed, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			terminal := func() {
				if feed == "provider" {
					output.observeProvider(gopevents.Error{Err: errors.New("repeated failure")})
				} else {
					output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnFailed, Payload: []byte(`{"error":"repeated failure"}`)})
				}
			}
			for i := 0; i < 3; i++ {
				terminal()
			}
			before := len(outputEvents(t, svc))
			var marker string
			if err := svc.trackTurnSubmission("s1", func() error { marker, _ = output.CurrentTurn(); terminal(); return nil }); err != nil {
				t.Fatal(err)
			}
			completion, ok := output.CompletedTurnDetails(marker)
			if !ok || !completion.Synthetic || completion.OutputKind != turnoutput.KindFailure {
				t.Fatalf("failure became clean final: %+v %v", completion, ok)
			}
			if after := len(outputEvents(t, svc)); after != before {
				t.Fatal("suppressed failure unexpectedly published")
			}
		})
	}
}
