package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestTeamDeliveryUsesIdleQueueAndNeverRetargets(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("retained", true, agentsessions.LiveStateIdle)
	h.setBusy("retained", true)
	if err := h.st.CreateSession(store.SessionRow{ID: "retained", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	d := teams.Delivery{IdempotencyKey: "team-key", From: "msg://agent/local/sender", Body: "wait for idle", Recipient: teams.Member{Actor: "msg://agent/local/worker", SessionID: "retained"}, Delivery: mesh.DeliveryAtIdle}
	if err := h.svc.QueueTeamDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	h.waitQuiet()
	rows, err := h.st.RoutingRepliesForParent(context.Background(), "team-delivery:"+d.IdempotencyKey)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	reply := rows[0]
	if reply.TargetSessionID != "retained" || reply.LogicalAgentID != "" || reply.State != store.RoutingReplyQueued {
		t.Fatal("queued turn lost retention", reply)
	}
	if h.rt.sendCallCount("retained") != 0 {
		t.Fatal("at-idle delivered while busy")
	}
	h.setBusy("retained", false)
	h.d.notify("retained")
	h.waitState(reply.ReplyID, store.RoutingReplyDelivered)
	if err = h.svc.QueueTeamDelivery(context.Background(), d); err != nil {
		t.Fatal("replayed queue receipt failed", err)
	}
	if h.rt.sendCallCount("retained") != 1 {
		t.Fatal("replay duplicated turn")
	}
	// A new retained delivery to an ended session cannot use a successor actor.
	if err = h.st.UpdateSessionState("retained", "killed", 0, nil); err != nil {
		t.Fatal(err)
	}
	d.IdempotencyKey = "ended-key"
	if err = h.svc.QueueTeamDelivery(context.Background(), d); !errors.Is(err, teamhost.ErrSessionGone) {
		t.Fatal("ended session accepted", err)
	}
	// The recorded successful key still replays after the retained session ends.
	d.IdempotencyKey = "team-key"
	if err = h.svc.QueueTeamDelivery(context.Background(), d); err != nil {
		t.Fatal("recorded outcome lost after session end", err)
	}
}
func TestTeamDeliveryDetachedAndUnsupportedPolicyRefuse(t *testing.T) {
	h := newReplyHarness(t)
	if err := h.st.CreateSession(store.SessionRow{ID: "detached", State: "detached"}, nil); err != nil {
		t.Fatal(err)
	}
	d := teams.Delivery{IdempotencyKey: "detach", From: "msg://agent/local/sender", Body: "work", Recipient: teams.Member{Actor: "msg://agent/local/worker", SessionID: "detached"}, Delivery: mesh.DeliveryAtIdle}
	if err := h.svc.QueueTeamDelivery(context.Background(), d); !errors.Is(err, teamhost.ErrSessionUnavailable) {
		t.Fatal("detached session discarded", err)
	}
	d.Delivery = "interrupt-now"
	if err := h.svc.QueueTeamDelivery(context.Background(), d); !errors.Is(err, teamhost.ErrInvalidRequest) {
		t.Fatal("unsupported policy substituted", err)
	}
}

func TestUnregisteredTeamAdaptersLeaveOrdinaryLaunchAndMessagingUnchanged(t *testing.T) {
	// Reuse the existing stub create and messaging harnesses; no adapter is
	// constructed, so additive migrations are the only team state present.
	svc, _ := idemHarness(t)
	first, err := svc.createKeyed("ordinary", "ordinary-digest", idemPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ParentSessionID.Valid || row.LogicalAgentID != "agent" {
		t.Fatal("ordinary launch metadata changed", row)
	}
	to, err := messaging.ParseURN("msg://user/local/reader")
	if err != nil {
		t.Fatal(err)
	}
	from, err := messaging.ParseURN("msg://user/local/writer")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: []byte(`{"body":"ordinary"}`)})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := svc.Store.MessagingStore().Get(context.Background(), sent.ID)
	if err != nil || string(retained.Payload) != `{"body":"ordinary"}` {
		t.Fatal(retained, err)
	}
	names, err := svc.Store.ListChannelNames(context.Background())
	if err != nil || len(names) != 0 {
		t.Fatal("unregistered team created channel", names, err)
	}
	for _, table := range []string{"team_port_intents", "team_runs", "team_host_routing", "team_host_intents"} {
		var count int
		if err = svc.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("ordinary path touched team state", table, count, err)
		}
	}
}
func TestTeamSessionCreateRetainsOwnBindingAndStopFencesUnstartedLaunch(t *testing.T) {
	rig := newCodexRig(t)
	created, err := rig.svc.CreateTeamSession(context.Background(), "team-port:owned", "codex-launch")
	if err != nil {
		t.Fatal(err)
	}
	row, err := rig.svc.Store.GetSession(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LogicalAgentID != "agent" {
		t.Fatal("team lost catalog sandbox identity", row.LogicalAgentID)
	}
	if err = rig.svc.LinkTeamSession(context.Background(), created.SessionID, "wrong-key", "parent"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("unowned session relinked", err)
	}
	if err = rig.svc.LinkTeamSession(context.Background(), created.SessionID, "team-port:owned", "parent"); err != nil {
		t.Fatal(err)
	}
	again, err := rig.svc.CreateTeamSession(context.Background(), "team-port:owned", "codex-launch")
	if err != nil || again.SessionID != created.SessionID {
		t.Fatal(again, err)
	}
	if err = rig.svc.StopTeamSession(context.Background(), created.SessionID); err != nil {
		t.Fatal(err)
	}
	row, err = rig.svc.Store.GetSession(created.SessionID)
	if err != nil || row.State != "killed" {
		t.Fatal("unstarted session not ended", row, err)
	}
	if _, err = rig.svc.LaunchSession(created.SessionID); err != nil {
		t.Fatal("keyed ended session replay failed", err)
	}
	if _, running := rig.svc.Manager.Get(created.SessionID); running {
		t.Fatal("stopped key launched a process")
	}
}

func TestTeamSessionUsesCatalogSandboxThroughRealService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := newCodexRig(t)
	rig.svc.Registry = registry.NewService(registry.NewStorage(rig.svc.Store.DB()))
	agent := rig.svc.Catalog.Agents["agent"]
	agent.Permissions.DefaultSandbox = "workspace-only"
	rig.svc.Catalog.Agents["agent"] = agent
	rig.svc.Catalog.SandboxProfiles = sandboxProfiles()
	provider := rig.svc.Catalog.Providers["codex-cli"]
	provider.Provider = "claude"
	provider.ID = "claude-pty"
	provider.RuntimeKind = config.RuntimeKindPTY
	rig.svc.Catalog.Providers[provider.ID] = provider
	ordinaryLaunch := rig.svc.Catalog.Launches["codex-launch"]
	ordinaryLaunch.Provider = provider.ID
	rig.svc.Catalog.Launches[ordinaryLaunch.ID] = ordinaryLaunch
	var profiles []sandbox.Profile
	rig.svc.factories[provider.ID] = func(plan *launch.Plan) (agentsessions.Runtime, error) {
		runtime, err := stub.New(plan)
		return teamProfileRuntime{Runtime: runtime, profiles: &profiles}, err
	}
	ordinary, err := rig.svc.CreateSession("codex-launch")
	if err != nil {
		t.Fatal(err)
	}
	// A caller-chosen host label must not change the ordinary lease path.
	actor := registry.LogicalAgentBindingTarget("agent")
	if _, err = rig.svc.Registry.LeaseBinding(context.Background(), actor, "foreign", "team", "caller-chosen", nil, registry.VisibilityTetherHosted, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = rig.svc.LaunchSession(ordinary.SessionID); err != nil {
		t.Fatal(err)
	}
	binding, err := rig.svc.Registry.CurrentBinding(context.Background(), actor)
	if err != nil || binding.SessionID != ordinary.SessionID {
		t.Fatal("ordinary launch lost its home binding", binding, err)
	}
	teamLaunch := rig.svc.Catalog.Launches["codex-launch"]
	teamLaunch.ID = "team-launch"
	rig.svc.Catalog.Launches[teamLaunch.ID] = teamLaunch
	team, err := rig.svc.CreateTeamSession(context.Background(), "team-port:nonce", teamLaunch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rig.svc.LaunchSession(team.SessionID); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].ID != "workspace-only" || profiles[1].ID != profiles[0].ID {
		t.Fatal("team sandbox silently downgraded", profiles)
	}
	plan, err := rig.svc.Store.GetLaunchPlan(team.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogicalAgentID != "agent" || !plan.TeamMember {
		t.Fatal("catalog policy identity lost", plan)
	}
	binding, err = rig.svc.Registry.CurrentBinding(context.Background(), actor)
	if err != nil || binding.SessionID != ordinary.SessionID {
		t.Fatal("team path overwrote ordinary actor binding", binding, err)
	}
	var retainedLaunch string
	if err = rig.svc.Store.DB().QueryRow(`SELECT launch_id FROM logical_agents WHERE id='agent'`).Scan(&retainedLaunch); err != nil || retainedLaunch != "codex-launch" {
		t.Fatal("team overwrote ordinary resume launch", retainedLaunch, err)
	}
	mirror, err := rig.svc.Store.GetClaudeSessionID("agent")
	if err != nil || mirror != "provider-ordinary" {
		t.Fatal("team overwrote ordinary provider session mirror", mirror, err)
	}
	if err = rig.svc.StopTeamSession(context.Background(), team.SessionID); err != nil {
		t.Fatal(err)
	}
	_ = rig.svc.Manager.Stop(context.Background(), ordinary.SessionID)
	_ = rig.svc.Manager.Shutdown(context.Background())
}

type teamProfileRuntime struct {
	agentsessions.Runtime
	profiles *[]sandbox.Profile
}

func (r teamProfileRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	*r.profiles = append(*r.profiles, opts.Profile)
	if opts.OnSessionID != nil {
		id := "provider-ordinary"
		if len(*r.profiles) > 1 {
			id = "provider-team"
		}
		opts.OnSessionID(id)
	}
	return r.Runtime.Start(ctx, opts)
}

type stopExitRuntime struct{ agentsessions.Runtime }

func (r stopExitRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	s, err := r.Runtime.Start(ctx, opts)
	return stopExitSession{Session: s, done: make(chan struct{})}, err
}

type stopExitSession struct {
	agentsessions.Session
	done chan struct{}
}

func (s stopExitSession) Wait() (int, error) {
	<-s.done
	return 137, errors.New("killed by our stop")
}
func (s stopExitSession) Stop(ctx context.Context) error {
	err := s.Session.Stop(ctx)
	close(s.done)
	return err
}
func TestStopTeamSessionAcknowledgesOwnExitErrorAndRetriesDetached(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	if _, err := seedLogicalAgents(svc.Store, map[string]config.Agent{"agent": {ID: "agent"}}); err != nil {
		t.Fatal(err)
	}
	svc.factories["stub"] = func(*launch.Plan) (agentsessions.Runtime, error) { return stopExitRuntime{Runtime: rt.Runtime}, nil }
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopTeamSession(context.Background(), id); err != nil {
		t.Fatal("own exit error blocked cleanup", err)
	}
	row, err := svc.Store.GetSession(id)
	if err != nil || !session.State(row.State).Terminal() || !row.ExitCode.Valid || row.ExitCode.Int64 != 137 {
		t.Fatal("stop not observed with its exit error", row, err)
	}
	if err = svc.Store.CreateSession(store.SessionRow{ID: "detached-team", State: "detached"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = svc.StopTeamSession(context.Background(), "detached-team"); !errors.Is(err, teamhost.ErrSessionUnavailable) {
		t.Fatal("detached treated as gone", err)
	}
}
