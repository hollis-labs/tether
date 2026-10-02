package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
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
