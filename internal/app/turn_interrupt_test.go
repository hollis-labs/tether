package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
)

type interruptRuntime struct{ session agentsessions.Session }

func (r interruptRuntime) ID() string                       { return "test" }
func (r interruptRuntime) Kind() string                     { return "test" }
func (r interruptRuntime) Caps() agentsessions.Capabilities { return agentsessions.Capabilities{} }
func (r interruptRuntime) Prepare(context.Context) error    { return nil }
func (r interruptRuntime) Start(context.Context, agentsessions.StartOptions) (agentsessions.Session, error) {
	return r.session, nil
}

type interruptSession struct {
	agentsessions.Session
	done  chan struct{}
	once  sync.Once
	input func(context.Context, []byte) error
}

func (s *interruptSession) Wait() (int, error) { <-s.done; return 0, nil }
func (s *interruptSession) Stop(context.Context) error {
	s.once.Do(func() { close(s.done) })
	return nil
}
func (s *interruptSession) Health() agentsessions.HealthStatus {
	return agentsessions.HealthStatus{Alive: true}
}
func (s *interruptSession) SendInput(ctx context.Context, data []byte) error {
	if s.input != nil {
		return s.input(ctx, data)
	}
	return nil
}

type cancellableSession struct {
	*interruptSession
	cancel func(context.Context) error
}

func (s *cancellableSession) InterruptTurn(ctx context.Context) error { return s.cancel(ctx) }

func interruptHarness(t *testing.T, cancel func(context.Context) error) (*Service, *sessionTurnOutput, *interruptSession) {
	t.Helper()
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	base := &interruptSession{done: make(chan struct{})}
	var session agentsessions.Session = base
	if cancel != nil {
		session = &cancellableSession{interruptSession: base, cancel: cancel}
	}
	svc.Manager = agentsessions.NewManager(nil)
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "s1", Runtime: interruptRuntime{session: session}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
	})
	return svc, output, base
}

func requireRefusal(t *testing.T, err error, reason TurnInterruptRefusalReason) {
	t.Helper()
	var refusal *TurnInterruptRefusal
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("error = %v, want refusal %s", err, reason)
	}
}

func TestCancelTurnAndWaitRequiresMatchingOutputAndAuditsActor(t *testing.T) {
	for _, feed := range []string{"native terminal", "native failure", "ACP", "empty terminal"} {
		t.Run(feed, func(t *testing.T) {
			var output *sessionTurnOutput
			svc, state, _ := interruptHarness(t, func(context.Context) error {
				switch feed {
				case "native failure":
					output.observeProvider(gopevents.Error{Err: errors.New("aborted")})
				case "ACP":
					output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "ACP-runtime-turn"})
				default:
					output.observeProvider(gopevents.Done{StopReason: "cancelled"}) //nolint:misspell // provider protocol spelling
				}
				return nil
			})
			output = state
			if err := svc.SendInput("s1", []byte("start")); err != nil {
				t.Fatal(err)
			}
			if feed == "ACP" {
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "ACP-runtime-turn"})
			}
			if feed != "ACP" && feed != "empty terminal" {
				output.observeProvider(gopevents.Delta{Text: "working"})
			}
			id, _ := output.CurrentTurn()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := svc.CancelTurnAndWait(ctx, "s1", "msg://user/local/alice")
			if err != nil {
				t.Fatal(err)
			}
			wantOutput := id
			if feed == "ACP" {
				wantOutput = "ACP-runtime-turn"
			}
			if result.TurnID != id || result.OutputTurnID != wantOutput {
				t.Fatalf("result = %+v", result)
			}
			if _, ok := svc.Manager.Get("s1"); !ok {
				t.Fatal("cancel stopped the session")
			}
			rows, err := svc.Store.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			var audit []events.TurnInterruptEvent
			for _, row := range rows {
				if row.Kind != events.KindSessionTurnInterruptRequested && row.Kind != events.KindSessionTurnInterruptCompleted {
					continue
				}
				var event events.TurnInterruptEvent
				if err := json.Unmarshal([]byte(row.PayloadJSON), &event); err != nil {
					t.Fatal(err)
				}
				audit = append(audit, event)
			}
			if len(audit) != 2 || audit[0].Result != "requested" || audit[1].Result != "completed" || audit[1].OutputTurnID != wantOutput {
				t.Fatalf("audit = %+v", audit)
			}
			for _, event := range audit {
				if event.Actor != "msg://user/local/alice" || event.SessionID != "s1" || event.TurnID != id {
					t.Fatalf("audit attribution = %+v", event)
				}
			}
		})
	}
}

func TestCancelTurnAndWaitAcknowledgementDoesNotCompleteTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, output, _ := interruptHarness(t, func(context.Context) error { cancel(); return nil })
	output.observeProvider(gopevents.Delta{Text: "working"})
	_, err := svc.CancelTurnAndWait(ctx, "s1", "actor")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acknowledgement returned success without terminal: %v", err)
	}
	if id, _ := output.CurrentTurn(); id == "" {
		t.Fatal("acknowledgement settled the turn")
	}
}

func TestCancelTurnAndWaitTypedRefusals(t *testing.T) {
	svc, output, _ := interruptHarness(t, nil)
	_, err := svc.CancelTurnAndWait(context.Background(), "s1", "actor")
	requireRefusal(t, err, TurnInterruptNoTurn)
	output.observeProvider(gopevents.Delta{Text: "working"})
	_, err = svc.CancelTurnAndWait(context.Background(), "s1", "actor")
	requireRefusal(t, err, TurnInterruptUnsupported)
	if !errors.Is(err, agentsessions.ErrInterruptUnsupported) {
		t.Fatal("unsupported lost shared runtime error")
	}
	_, err = svc.CancelTurnAndWait(context.Background(), "missing", "actor")
	if !errors.Is(err, agentsessions.ErrSessionNotRunning) {
		t.Fatal(err)
	}
}

func TestCancelTurnAndWaitProvisionalSubmissionNeverCallsRuntime(t *testing.T) {
	var calls atomic.Int32
	svc, output, base := interruptHarness(t, func(context.Context) error { calls.Add(1); return nil })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseInput := func() { once.Do(func() { close(release) }) }
	defer releaseInput()
	base.input = func(context.Context, []byte) error { close(entered); <-release; return nil }
	submitted := make(chan error, 1)
	go func() { submitted <- svc.SendInput("s1", []byte("start")) }()
	<-entered
	_, err := svc.CancelTurnAndWait(context.Background(), "s1", "actor")
	requireRefusal(t, err, TurnInterruptNotStarted)
	if calls.Load() != 0 {
		t.Fatal("cancel reached a runtime before acceptance")
	}
	releaseInput()
	if err := <-submitted; err != nil {
		t.Fatal(err)
	}
	id, _ := output.CurrentTurn()
	if !output.TurnAccepted(id) {
		t.Fatal("successful submission never accepted")
	}
}

func TestCancelTurnAndWaitGateRejectsSnapshotSuccessor(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(map[bool]string{false: "ended", true: "successor"}[successor], func(t *testing.T) {
			var calls atomic.Int32
			svc, output, _ := interruptHarness(t, func(context.Context) error { calls.Add(1); return nil })
			output.observeProvider(gopevents.Delta{Text: "first"})
			intended, _ := output.CurrentTurn()
			unlock := output.LockSubmission()
			returned := make(chan error, 1)
			go func() {
				_, err := svc.cancelTurnAndWait(context.Background(), "s1", "actor", output, intended)
				returned <- err
			}()
			output.observeProvider(gopevents.Done{})
			if successor {
				// Emulate the winning successor submission while owning the gate.
				output.mu.Lock()
				output.ensureTurn()
				output.accepted = true
				output.mu.Unlock()
			}
			next, _ := output.CurrentTurn()
			unlock()
			err := <-returned
			want := TurnInterruptNoTurn
			if successor {
				want = TurnInterruptSuperseded
			}
			requireRefusal(t, err, want)
			if calls.Load() != 0 {
				t.Fatal("cancel reached successor runtime turn")
			}
			if current, _ := output.CurrentTurn(); current != next {
				t.Fatal("successor marker was modified")
			}
		})
	}
}

func TestCancelTurnAndWaitRejectsSettlementWithoutOutput(t *testing.T) {
	var output *sessionTurnOutput
	svc, state, _ := interruptHarness(t, func(context.Context) error { output.flush(); return nil })
	output = state
	if err := svc.SendInput("s1", []byte("no output")); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CancelTurnAndWait(context.Background(), "s1", "actor")
	requireRefusal(t, err, TurnInterruptSuperseded)
}

func TestCancelTurnAndWaitRejectsDifferentACPOutput(t *testing.T) {
	var output *sessionTurnOutput
	svc, state, _ := interruptHarness(t, func(context.Context) error {
		output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "different-runtime-turn"})
		output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "different-runtime-turn"})
		return nil
	})
	output = state
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "intended-runtime-turn"})
	_, err := svc.CancelTurnAndWait(context.Background(), "s1", "actor")
	requireRefusal(t, err, TurnInterruptSuperseded)
}

type completingAcceptanceState struct {
	TurnOutputState
	finish func()
}

func (s completingAcceptanceState) TurnAccepted(id string) bool {
	s.finish()
	return s.TurnOutputState.TurnAccepted(id)
}

func TestCancelTurnAndWaitCompletionDuringAcceptanceCheck(t *testing.T) {
	var calls atomic.Int32
	svc, output, _ := interruptHarness(t, func(context.Context) error { calls.Add(1); return nil })
	output.observeProvider(gopevents.Delta{Text: "working"})
	intended, _ := output.CurrentTurn()
	state := completingAcceptanceState{TurnOutputState: output, finish: func() { output.observeProvider(gopevents.Done{}) }}
	_, err := svc.cancelTurnAndWait(context.Background(), "s1", "actor", state, intended)
	requireRefusal(t, err, TurnInterruptNoTurn)
	if calls.Load() != 0 {
		t.Fatal("completed turn reached runtime cancellation")
	}
}

func TestCancelTurnAndWaitAuditFailurePreventsCancel(t *testing.T) {
	var calls atomic.Int32
	svc, output, _ := interruptHarness(t, func(context.Context) error { calls.Add(1); return nil })
	output.observeProvider(gopevents.Delta{Text: "working"})
	if _, err := svc.Store.DB().Exec(`CREATE TRIGGER reject_interrupt BEFORE INSERT ON events BEGIN SELECT RAISE(FAIL,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelTurnAndWait(context.Background(), "s1", "actor"); err == nil {
		t.Fatal("unaudited cancel accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("runtime canceled despite failed intent audit")
	}
}

func TestCancelTurnAndWaitReleasesGateBeforeTerminal(t *testing.T) {
	acknowledged := make(chan struct{})
	svc, output, base := interruptHarness(t, func(context.Context) error {
		close(acknowledged)
		return nil
	})
	output.observeProvider(gopevents.Delta{Text: "working"})
	id, _ := output.CurrentTurn()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := svc.CancelTurnAndWait(ctx, "s1", "actor"); returned <- err }()
	<-acknowledged
	steered := make(chan struct{})
	base.input = func(context.Context, []byte) error { close(steered); return nil }
	inputDone := make(chan error, 1)
	go func() { inputDone <- svc.SendInput("s1", []byte("steer")) }()
	select {
	case <-steered:
	case <-ctx.Done():
		t.Fatal("completion wait retained submission gate")
	}
	if err := <-inputDone; err != nil {
		t.Fatal(err)
	}
	if current, _ := output.CurrentTurn(); current != id {
		t.Fatal("steering replaced the canceled marker")
	}
	output.observeProvider(gopevents.Done{StopReason: "cancelled"}) //nolint:misspell // provider protocol spelling
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
}

func TestCancelTurnAndWaitUnblocksAcceptedSendInput(t *testing.T) {
	var output *sessionTurnOutput
	release := make(chan struct{})
	var once sync.Once
	releaseInput := func() { once.Do(func() { close(release) }) }
	defer releaseInput()
	svc, state, base := interruptHarness(t, func(context.Context) error {
		output.observeProvider(gopevents.Done{StopReason: "cancelled"}) //nolint:misspell // provider protocol spelling
		releaseInput()
		return nil
	})
	output = state
	entered := make(chan struct{})
	base.input = func(context.Context, []byte) error {
		output.observeProvider(gopevents.Delta{Text: "accepted but input call still blocked"})
		close(entered)
		<-release
		return nil
	}
	inputDone := make(chan error, 1)
	go func() { inputDone <- svc.SendInput("s1", []byte("start")) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := svc.CancelTurnAndWait(ctx, "s1", "actor"); returned <- err }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cancellation waited behind accepted SendInput")
	}
	if err := <-inputDone; err != nil {
		t.Fatal(err)
	}
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatal("successful blocking submission resurrected completed marker")
	}
}
