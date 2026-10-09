package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
)

type markerRuntime struct {
	agentsessions.Runtime
	caps    agentsessions.Capabilities
	onInput func()
	onStart func()
}

func (r *markerRuntime) Caps() agentsessions.Capabilities { return r.caps }
func (r *markerRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if r.onStart != nil {
		r.onStart()
	}
	session, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &markerSession{Session: session, onInput: r.onInput}, nil
}

type markerSession struct {
	agentsessions.Session
	onInput func()
}

func (s *markerSession) SendInput(ctx context.Context, data []byte) error {
	s.onInput()
	return s.Session.SendInput(ctx, data)
}
func (s *markerSession) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if method == "turn/start" {
		s.onInput()
	}
	if method == "thread/start" {
		return json.RawMessage(`{"thread":{"id":"thread"}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func TestSubmissionMarkersThroughPublicInputPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		caps       agentsessions.Capabilities
		raw        bool
		wantMarker bool
	}{
		{"streaming turn", agentsessions.Capabilities{StreamingStdio: true}, false, true},
		{"RPC turn", agentsessions.Capabilities{JsonRpcStdio: true}, false, true},
		{"default turn", agentsessions.Capabilities{}, false, true},
		{"semantic PTY turn", agentsessions.Capabilities{PTY: true}, false, true},
		{"raw semantic input", agentsessions.Capabilities{}, true, true},
		{"raw PTY keystroke", agentsessions.Capabilities{PTY: true}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			svc.Manager = agentsessions.NewManager(nil)
			base, err := stub.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			runtime := &markerRuntime{Runtime: base, caps: tc.caps, onInput: func() {
				calls++
				id, done := output.CurrentTurn()
				if (id != "") != tc.wantMarker || (done != nil) != tc.wantMarker {
					t.Fatal("submission marker absent or keystroke primed it")
				}
				if id != "" && output.TurnAccepted(id) {
					t.Fatal("provisional marker accepted before input")
				}
			}}
			if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "s1", Runtime: runtime}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = svc.Manager.Stop(context.Background(), "s1") }()
			if tc.raw {
				err = svc.SendInput("s1", []byte("input"))
			} else {
				err = svc.SendTurn(context.Background(), "s1", "input")
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("runtime input was not observed")
			}
			if id, _ := output.CurrentTurn(); tc.wantMarker && !output.TurnAccepted(id) {
				t.Fatal("successful submission not accepted")
			}
		})
	}
}

func TestBootSubmissionMarkerExistsBeforeRuntimeStart(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	plan.RuntimeKind = "streaming-stdio"
	plan.BootMode = "stdin"
	plan.BootPrompt = "boot turn"
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	entered := false
	wrapper := &markerRuntime{Runtime: rt.Runtime, caps: agentsessions.Capabilities{StreamingStdio: true}, onStart: func() {
		entered = true
		state, ok := svc.SessionTurnOutputState(id)
		if !ok {
			t.Fatal("boot has no collector")
		}
		marker, done := state.CurrentTurn()
		if marker == "" || done == nil || state.TurnAccepted(marker) {
			t.Fatal("boot marker not provisional at runtime entry")
		}
	}}
	svc.factories["stub"] = func(*launch.Plan) (agentsessions.Runtime, error) { return wrapper, nil }
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	if !entered {
		t.Fatal("runtime not started")
	}
	state, ok := svc.SessionTurnOutputState(id)
	if !ok {
		t.Fatal("collector lost")
	}
	marker, _ := state.CurrentTurn()
	if !state.TurnAccepted(marker) {
		t.Fatal("boot not accepted")
	}
}

func TestCanceledSendTurnDoesNotPrimeOrEnterRuntimeBehindGate(t *testing.T) {
	for _, caps := range []agentsessions.Capabilities{{}, {StreamingStdio: true}, {JsonRpcStdio: true}} {
		t.Run(fmt.Sprintf("%+v", caps), func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			svc.Manager = agentsessions.NewManager(nil)
			base, err := stub.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			rt := &markerRuntime{Runtime: base, caps: caps, onInput: func() { entered <- struct{}{} }}
			if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "s1", Runtime: rt}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = svc.Manager.Stop(context.Background(), "s1") }()
			unlock := output.LockSubmission()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			returned := make(chan error, 1)
			go func() { returned <- svc.SendTurn(ctx, "s1", "input") }()
			select {
			case err := <-returned:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				unlock()
				t.Fatal("request cancellation did not release gate waiter")
			}
			unlock()
			if id, _ := output.CurrentTurn(); id != "" {
				t.Fatal("canceled request left a marker")
			}
			select {
			case <-entered:
				t.Fatal("canceled request entered runtime")
			default:
			}
			if !output.submissionGate.TryLock() {
				t.Fatal("canceled waiter retained gate")
			}
			output.submissionGate.Unlock()
		})
	}
}

func TestSubmissionCancellationAfterGateBeforeRuntimeEntry(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output.mu.Lock()
	returned := make(chan error, 1)
	entered := false
	go func() {
		returned <- svc.trackTurnSubmissionContext(ctx, "s1", func() error { entered = true; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for output.submissionGate.TryLock() {
		output.submissionGate.Unlock()
		if time.Now().After(deadline) {
			output.mu.Unlock()
			t.Fatal("submission never acquired gate")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	output.mu.Unlock()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("error: %v", err)
	}
	if entered {
		t.Fatal("canceled submission entered runtime after gate acquisition")
	}
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatal("canceled provisional marker retained")
	}
}

type cancellingObserverRuntime struct {
	*markerRuntime
	cancel context.CancelFunc
}

func (r *cancellingObserverRuntime) SetEventObserver(func(runtimeevents.Event)) { r.cancel() }
func TestBootSubmissionHonorsCancellationAtCollectorWiring(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	plan.RuntimeKind = "streaming-stdio"
	plan.BootMode, plan.BootPrompt = "stdin", "boot turn"
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := false
	wrapper := &cancellingObserverRuntime{markerRuntime: &markerRuntime{Runtime: rt.Runtime, caps: agentsessions.Capabilities{StreamingStdio: true}, onStart: func() { entered = true }}, cancel: cancel}
	svc.factories["stub"] = func(*launch.Plan) (agentsessions.Runtime, error) { return wrapper, nil }
	if _, err := svc.LaunchSessionWithContext(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("launch error: %v", err)
	}
	if entered {
		t.Fatal("boot entered runtime despite canceled caller")
	}
	if _, ok := svc.SessionTurnOutputState(id); ok {
		t.Fatal("canceled boot retained collector")
	}
}
