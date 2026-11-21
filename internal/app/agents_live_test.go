package app

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agentlive"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestAgentsLiveMergesSessionsAndKitSeats(t *testing.T) {
	svc, _, _, _ := credentialLaunch(t)
	ctx := context.Background()
	plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", LogicalAgentID: "agent"}
	for _, row := range []store.SessionRow{
		{ID: "run-1", LogicalAgentID: "agent", Workspace: t.TempDir(), State: "running"},
		{ID: "ended-1", LogicalAgentID: "agent", Workspace: t.TempDir(), State: "completed"},
	} {
		if err := svc.Store.CreateSession(row, plan); err != nil {
			t.Fatal(err)
		}
	}
	// A kit seat registers through the same entry point the hook uses.
	urn := "msg://agent/agent-mux/kit-lead"
	if _, err := svc.RegisterExternalCorrelation(ctx, api.CorrelationRegisterRequest{URN: urn, Session: "kit-sess", Host: "sol", DisplayName: "Kit Lead", Summary: "booting"}); err != nil {
		t.Fatal(err)
	}
	// A session id that is a Tether session is never recorded as a kit seat.
	if _, err := svc.RegisterExternalCorrelation(ctx, api.CorrelationRegisterRequest{URN: urn, Session: "run-1"}); err != nil {
		t.Fatal(err)
	}
	q, _ := agentlive.ParseQuery("", "", true, 0, 0, "")
	watch := agentlive.NewTracker()
	rel := watch.Watch(urn)
	got, err := svc.AgentsLive(ctx, q, watch)
	if err != nil {
		t.Fatal(err)
	}
	var sessions, seats int
	for _, r := range got.Agents {
		switch r.Kind {
		case agentlive.KindSession:
			sessions++
			if r.SessionID != "run-1" || r.Agent.URN != "msg://agent/agent-mux/agent" || r.State != agentlive.StateUnknown || !r.Stale {
				t.Errorf("session row (no runtime, no activity must be unknown+stale): %+v", r)
			}
		case agentlive.KindKitSeat:
			seats++
			if r.Agent.URN != urn || r.Host != "sol" || r.Agent.Name != "Kit Lead" || r.LastActivity != "booting" || r.SessionID != "kit-sess" || r.Stale || r.PresenceSource != "watcher" {
				t.Errorf("seat row: %+v", r)
			}
		}
	}
	if sessions != 1 || seats != 1 {
		t.Fatalf("sessions=%d seats=%d (terminal session excluded): %+v", sessions, seats, got.Agents)
	}
	rel()

	// Default filters hide the stale session; kind=kit_seat skips session reads.
	def, _ := agentlive.ParseQuery("", "", false, 0, 0, "")
	if got, _ := svc.AgentsLive(ctx, def, nil); got.Count != 1 || got.Agents[0].Kind != agentlive.KindKitSeat {
		t.Errorf("default hides stale: %+v", got.Agents)
	}
	// A seat's presence decays: backdate its registration past the threshold.
	if _, err := svc.Store.DB().Exec(`UPDATE kit_seats SET last_seen_at=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.AgentsLive(ctx, def, nil); got.Count != 0 {
		t.Errorf("old registration alone must be stale: %+v", got.Agents)
	}
	long, _ := agentlive.ParseQuery("", agentlive.KindKitSeat, false, 7200, 0, "")
	if got, _ := svc.AgentsLive(ctx, long, nil); got.Count != 1 {
		t.Errorf("custom threshold keeps it fresh: %+v", got.Agents)
	}
}

func TestKitSeatReregisterKeepsKnownFacts(t *testing.T) {
	svc, _, _, _ := credentialLaunch(t)
	ctx := context.Background()
	now := time.Now()
	k := store.KitSeat{AgentURN: "msg://agent/agent-mux/x", Session: "s", Host: "h", WorkstreamID: "w"}
	if err := svc.Store.UpsertKitSeat(ctx, k, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.UpsertKitSeat(ctx, store.KitSeat{AgentURN: k.AgentURN, Session: "s", TaskID: "T"}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Store.ListKitSeats(ctx, now.Add(-time.Hour), 0)
	if err != nil || len(rows) != 1 || rows[0].Host != "h" || rows[0].WorkstreamID != "w" || rows[0].TaskID != "T" || !rows[0].LastSeenAt.After(rows[0].FirstSeenAt) {
		t.Fatalf("%+v %v", rows, err)
	}
	// Retention prunes registrations nobody refreshed.
	if err := svc.Store.UpsertKitSeat(ctx, store.KitSeat{AgentURN: "msg://agent/agent-mux/y", Session: "s2"}, now.Add(store.KitSeatRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rows, _ := svc.Store.ListKitSeats(ctx, time.Time{}, 0); len(rows) != 1 || rows[0].Session != "s2" {
		t.Fatalf("pruned: %+v", rows)
	}
}
