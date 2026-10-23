package app

import (
	"context"
	"errors"
	"testing"

	agentsessions "github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func instSession(t *testing.T, st *store.Store, id, agent string, state session.State, parent string) {
	t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: agent, ProviderID: "stub", Workspace: t.TempDir(), State: string(state)}
	if parent != "" {
		row.ParentSessionID.String, row.ParentSessionID.Valid = parent, true
	}
	if err := st.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatal(err)
	}
}

func instance(agent, sid string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: agent, SubID: sid}
}

// CW-20261023-0018, option C(a): an agent instance address names one session, derived
// from the session row; nothing falls back.
func TestResolveInstanceSession(t *testing.T) {
	st, _ := newWakeHarness(t)
	rt := newFakeRuntime()
	instSession(t, st, "a1", "general", session.StateRunning, "")
	instSession(t, st, "o1", "other", session.StateRunning, "")
	instSession(t, st, "e1", "general", session.StateCompleted, "")
	instSession(t, st, "e2", "general", session.StateRunning, "e1") // e1's resumed successor
	instSession(t, st, "l1", "general", session.StateLaunching, "")
	rt.setAlive("a1", true, agentsessions.LiveStateIdle)
	rt.setAlive("o1", true, agentsessions.LiveStateIdle)
	rt.setAlive("e2", true, agentsessions.LiveStateIdle)
	seam := rt.seam()
	ctx := context.Background()

	if id, err := resolveInstanceSession(ctx, st, seam, instance("general", "a1")); err != nil || id != "a1" {
		t.Fatalf("running instance = %q %v, want a1", id, err)
	}
	// A session of another logical agent, an unknown id and a bare address are unknown,
	// not told apart.
	for name, to := range map[string]messaging.Address{
		"other agent's session": instance("general", "o1"),
		"unknown session":       instance("general", "nope"),
		"another authority":     {Kind: messaging.KindAgent, Authority: "elsewhere", ID: "general", SubID: "a1"},
		"bare agent address":    {Kind: messaging.KindAgent, Authority: "agent-mux", ID: "general"},
	} {
		if id, err := resolveInstanceSession(ctx, st, seam, to); !errors.Is(err, api.ErrInstanceUnknown) || id != "" {
			t.Errorf("%s = %q %v, want ErrInstanceUnknown", name, id, err)
		}
	}
	// An ended session names its resumed successor and resolves to nothing.
	id, err := resolveInstanceSession(ctx, st, seam, instance("general", "e1"))
	var ended *api.InstanceEndedError
	if id != "" || !errors.As(err, &ended) || ended.SessionID != "e1" || ended.SupersededBy != "e2" {
		t.Fatalf("ended instance = %q %v, want InstanceEndedError e1 superseded_by e2", id, err)
	}
	// An ended session with no successor says so without inventing one.
	instSession(t, st, "e3", "general", session.StateFailed, "")
	if _, err := resolveInstanceSession(ctx, st, seam, instance("general", "e3")); !errors.As(err, &ended) || ended.SupersededBy != "" {
		t.Fatalf("ended without successor: %v", err)
	}
	// A launching session has not ended: stored mail waits for it.
	if _, err := resolveInstanceSession(ctx, st, seam, instance("general", "l1")); !errors.Is(err, api.ErrBoundSessionNotRunning) {
		t.Fatalf("launching instance: %v, want ErrBoundSessionNotRunning", err)
	}
}

// The blocker (Archie's requirement i): mail to an instance address wakes that session,
// not the logical agent's bound home session, and the home binding does not fence it.
func TestInstanceAddressWakesItsOwnSessionNotTheBoundOne(t *testing.T) {
	st, reg := newWakeHarness(t)
	rt := newFakeRuntime()
	instSession(t, st, "home", "general", session.StateRunning, "")
	instSession(t, st, "other", "general", session.StateRunning, "")
	rt.setAlive("home", true, agentsessions.LiveStateIdle)
	rt.setAlive("other", true, agentsessions.LiveStateIdle)
	if _, err := reg.LeaseBinding(context.Background(), registry.LogicalAgentBindingTarget("general"), "home", "local", "home", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatal(err)
	}
	seam := rt.seam()
	ctx := context.Background()

	to := instance("general", "other")
	if id, err := resolveWakeTarget(ctx, st, reg, seam, to); err != nil || id != "other" {
		t.Fatalf("sweep target for the instance = %q %v, want other", id, err)
	}
	env := sendMessage(t, st, to)
	if out := attemptWake(ctx, st, reg, seam, env.ID, to, "other", "wake"); !out.Delivered || out.Reason != "" {
		t.Fatalf("instance wake = %+v, want delivered (the home binding must not fence it)", out)
	}
	if rt.sendCallCount("other") != 1 || rt.sendCallCount("home") != 0 {
		t.Fatalf("turns: other=%d home=%d, want 1 and 0", rt.sendCallCount("other"), rt.sendCallCount("home"))
	}
	// The bare address still means the bound home session.
	bare := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "general"}
	if id, err := resolveWakeTarget(ctx, st, reg, seam, bare); err != nil || id != "home" {
		t.Fatalf("bare address target = %q %v, want home", id, err)
	}
}

// An ended or unknown instance is parked by the sweep, never redirected to the bound
// session or the successor.
func TestSweepNeverRedirectsAnEndedInstance(t *testing.T) {
	st, reg := newWakeHarness(t)
	rt := newFakeRuntime()
	instSession(t, st, "home", "general", session.StateRunning, "")
	instSession(t, st, "gone", "general", session.StateCompleted, "")
	instSession(t, st, "succ", "general", session.StateRunning, "gone")
	rt.setAlive("home", true, agentsessions.LiveStateIdle)
	rt.setAlive("succ", true, agentsessions.LiveStateIdle)
	if _, err := reg.LeaseBinding(context.Background(), registry.LogicalAgentBindingTarget("general"), "home", "local", "home", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatal(err)
	}
	seam := rt.seam()
	for _, sid := range []string{"gone", "nope"} {
		sendMessage(t, st, instance("general", sid))
	}
	if n, err := runWakeSweep(context.Background(), st, reg, seam, nil); err != nil || n != 0 {
		t.Fatalf("sweep attempted %d (err %v), want none", n, err)
	}
	for _, id := range []string{"home", "succ", "gone"} {
		if rt.sendCallCount(id) != 0 {
			t.Fatalf("session %s was woken by mail to an ended or unknown instance", id)
		}
	}
}
