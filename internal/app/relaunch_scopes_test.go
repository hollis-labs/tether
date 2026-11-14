package app

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func (r *identityRig) withBus() {
	if r.svc.Bus == nil {
		r.svc.Bus = events.NewBus(events.BusOptions{Persister: r.db})
	}
}

func (r *identityRig) narrowedEvents(sessionID string) []map[string]any {
	r.t.Helper()
	rows, err := r.db.DB().Query(`SELECT payload_json FROM events WHERE kind=? AND session_id=?`, KindSessionScopesNarrowed, sessionID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		_ = rows.Scan(&raw)
		var p map[string]any
		_ = json.Unmarshal([]byte(raw), &p)
		out = append(out, p)
	}
	return out
}

func (r *identityRig) attribution(id string) string {
	r.t.Helper()
	var a string
	if err := r.db.DB().QueryRow(`SELECT coalesce(launch_attribution,'') FROM principals WHERE kind='session' AND session_id=? AND revoked_at IS NULL`, id).Scan(&a); err != nil {
		r.t.Fatal(err)
	}
	return a
}

// CW-20261114-0038: a resume of the same seat keeps what the seat's previous
// session held, whoever triggers it; a first launch is still bounded by the
// launcher, and a narrowing publishes session.scopes_narrowed.
func TestRelaunchOfTheSameSeatKeepsItsGrant(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	if len(defaults) < 2 {
		t.Fatalf("test needs at least two default scopes, have %v", defaults)
	}
	one := defaults[0]
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.withBus()

	// The seat's first session, launched by a principal holding everything.
	if _, err := r.launchAs(launcherWith("*"), "sess-parent", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-parent"); !slices.Equal(got, defaults) {
		t.Fatalf("parent scopes %v", got)
	}

	// A first launch by a narrow launcher is narrowed, and says so.
	if _, err := r.launchAs(launcherWith(one), "sess-fresh", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-fresh"); !slices.Equal(got, []string{one}) {
		t.Fatalf("fresh launch by a narrow launcher: %v", got)
	}
	ev := r.narrowedEvents("sess-fresh")
	if len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher || len(ev[0]["dropped"].([]any)) != len(defaults)-1 {
		t.Fatalf("narrowed events = %v", ev)
	}

	// The same launcher resuming the same seat keeps the parent's grant.
	resume := withLaunchOrigin(launcherWith(one), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-parent"})
	spec, err := r.launchAs(resume, "sess-resumed", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, defaults) || !slices.Equal(r.storedScopes("sess-resumed"), defaults) {
		t.Fatalf("resume of the same seat: spec %v, credential %v; want %v", spec.MCPScopes, r.storedScopes("sess-resumed"), defaults)
	}
	if a := r.attribution("sess-resumed"); a != string(AttributionOrigin) {
		t.Fatalf("attribution %q, want %q (the parent grant vouched)", a, AttributionOrigin)
	}
	if ev := r.narrowedEvents("sess-resumed"); len(ev) != 0 {
		t.Fatalf("unexpected narrowing on the same-seat resume: %v", ev)
	}

	// Nobody gains what the seat never held: resuming a narrowed session keeps it narrow.
	resumeFresh := withLaunchOrigin(launcherWith("*"), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-fresh"})
	if _, err := r.launchAs(resumeFresh, "sess-from-fresh", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-from-fresh"); !slices.Equal(got, []string{one}) {
		t.Fatalf("resume of a narrow session widened to %v", got)
	}

	// A resume naming another seat's session gets no grant from it: the launcher bounds it.
	if _, err := r.db.DB().Exec(`UPDATE sessions SET logical_agent_id='someone-else' WHERE id='sess-parent'`); err != nil {
		t.Fatal(err)
	}
	other := withLaunchOrigin(launcherWith(one), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-parent"})
	if _, err := r.launchAs(other, "sess-cross", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-cross"); !slices.Equal(got, []string{one}) {
		t.Fatalf("cross-seat resume took the other seat's grant: %v", got)
	}
}

// relaunchAs creates a session the way POST /sessions with continuity "relaunch"
// does (the parent is the newest earlier session of the agent) and launches it.
func (r *identityRig) relaunchAs(ctx context.Context, id string) *store.SessionRow {
	r.t.Helper()
	return r.relaunchWith(ctx, id, CreateSessionInput{Continuity: "relaunch"})
}

// relaunchWith is relaunchAs with the caller's create payload: the plan is
// stamped as BuildLaunchPlan stamps it, stored, and read back for the launch as
// LaunchSession reads it.
func (r *identityRig) relaunchWith(ctx context.Context, id string, in CreateSessionInput) *store.SessionRow {
	r.t.Helper()
	plan := &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", Continuity: in.Continuity}
	stampSeatRelaunch(plan, in)
	row := store.SessionRow{ID: id, WorkerID: r.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: r.t.TempDir(), State: "created"}
	if err := r.svc.createContinued(row, plan, nil); err != nil {
		r.t.Fatal(err)
	}
	stored, err := r.db.GetSession(id)
	if err != nil {
		r.t.Fatal(err)
	}
	storedPlan, err := r.db.GetLaunchPlan(id)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.svc.launchWorkerSession(ctx, stored, storedPlan); err != nil {
		r.t.Fatal(err)
	}
	select {
	case <-r.specs:
	case <-time.After(3 * time.Second):
		r.t.Fatal("worker never received the launch")
	}
	return stored
}

// CW-20261114-0038 follow-up: every catalog seat runs on a worker, where resume
// is off, so Tachyon's restart is stop + create(continuity=relaunch). That
// create keeps the grant of the seat's ended previous session; a relaunch whose
// newest earlier session is still running is bounded by the launcher.
func TestRelaunchCreateOfTheSameSeatKeepsItsGrant(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	one := defaults[0]
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.withBus()
	if _, err := r.launchAs(launcherWith("*"), "sess-parent", ""); err != nil {
		t.Fatal(err)
	}
	// The stop: the session ends and its credential is revoked with it.
	if _, err := r.db.DB().Exec(`UPDATE sessions SET state='completed' WHERE id='sess-parent'`); err != nil {
		t.Fatal(err)
	}
	if err := identity.NewStore(r.db.DB()).RevokeSession(context.Background(), "sess-parent"); err != nil {
		t.Fatal(err)
	}

	row := r.relaunchAs(launcherWith(one), "sess-relaunched")
	if row.Intent != "resume" || row.ParentSessionID.String != "sess-parent" {
		t.Fatalf("relaunch lineage: intent %q parent %v", row.Intent, row.ParentSessionID)
	}
	if got := r.storedScopes("sess-relaunched"); !slices.Equal(got, defaults) {
		t.Fatalf("relaunch by a narrow device: %v, want %v", got, defaults)
	}
	if a := r.attribution("sess-relaunched"); a != string(AttributionOrigin) {
		t.Fatalf("attribution %q, want %q", a, AttributionOrigin)
	}
	if ev := r.narrowedEvents("sess-relaunched"); len(ev) != 0 {
		t.Fatalf("unexpected narrowing: %v", ev)
	}

	// sess-relaunched is now the newest and has not ended: a second relaunch
	// cannot copy a live seat's grant.
	r.relaunchAs(launcherWith(one), "sess-clone")
	if got := r.storedScopes("sess-clone"); !slices.Equal(got, []string{one}) {
		t.Fatalf("relaunch beside a running seat took its grant: %v", got)
	}
	if ev := r.narrowedEvents("sess-clone"); len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher {
		t.Fatalf("narrowed events = %v", ev)
	}

	// A stop that is requested but not yet reported by the worker counts as
	// ended: Tachyon relaunches as soon as POST /stop returns. The parent's grant
	// still caps it, so relaunching the narrow sess-clone stays narrow.
	if r.svc.stops == nil {
		r.svc.stops = &stopRequests{}
	}
	r.svc.stops.mark("sess-clone")
	r.relaunchAs(launcherWith("*"), "sess-after-stop")
	if got := r.storedScopes("sess-after-stop"); !slices.Equal(got, []string{one}) {
		t.Fatalf("relaunch after stopping a narrow session widened to %v", got)
	}
	if a := r.attribution("sess-after-stop"); a != string(AttributionOrigin) {
		t.Fatalf("stop-requested parent: attribution %q, want %q", a, AttributionOrigin)
	}
}

// @photon-review on #553: a relaunch create also accepts caller payloads, so an
// operate-only device could otherwise run the seat's previous grant under its
// own boot prompt, agent, overrides, injected files or worker. Each of those
// means the launcher basis; PromptAppend (a turn's worth of notes) does not.
func TestRelaunchCreateWithCallerPayloadIsBoundedByTheLauncher(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	one := defaults[0]
	cases := []struct {
		name       string
		in         CreateSessionInput
		otherWkr   bool // the parent ran on another worker
		wantParent bool
	}{
		{"plain relaunch", CreateSessionInput{}, false, true},
		{"prompt_append", CreateSessionInput{BootPromptAppend: "relaunch note"}, false, true},
		{"boot_prompt", CreateSessionInput{BootPromptOverride: "you are someone else"}, false, false},
		{"agent_inline", CreateSessionInput{AgentInline: `{"id":"x"}`}, false, false},
		{"agent_file", CreateSessionInput{AgentFile: "/tmp/agent.yaml"}, false, false},
		{"boot_profile", CreateSessionInput{BootProfileFile: "/tmp/profile.yaml"}, false, false},
		{"override", CreateSessionInput{Override: `{"system_prompt":"x"}`}, false, false},
		{"injection", CreateSessionInput{Injection: `{"native_files":[]}`}, false, false},
		{"named worker is the parent's", CreateSessionInput{Worker: "fake-worker"}, false, true},
		{"named worker is not the parent's", CreateSessionInput{Worker: "fake-worker"}, true, false},
		{"unnamed worker, catalog placed elsewhere", CreateSessionInput{}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
			r.withBus()
			if _, err := r.launchAs(launcherWith("*"), "sess-parent", ""); err != nil {
				t.Fatal(err)
			}
			parentWorker := r.worker
			if c.otherWkr {
				parentWorker = "worker_other"
			}
			if _, err := r.db.DB().Exec(`UPDATE sessions SET state='completed', worker_id=? WHERE id='sess-parent'`, parentWorker); err != nil {
				t.Fatal(err)
			}
			in := c.in
			in.Continuity = "relaunch"
			r.relaunchWith(launcherWith(one), "sess-relaunched", in)
			got := r.storedScopes("sess-relaunched")
			if c.wantParent {
				if !slices.Equal(got, defaults) || r.attribution("sess-relaunched") != string(AttributionOrigin) {
					t.Fatalf("want the parent grant %v (origin), got %v (%s)", defaults, got, r.attribution("sess-relaunched"))
				}
				return
			}
			if !slices.Equal(got, []string{one}) {
				t.Fatalf("a reshaped relaunch kept the parent grant: %v", got)
			}
			if ev := r.narrowedEvents("sess-relaunched"); len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher {
				t.Fatalf("narrowed events = %v", ev)
			}
		})
	}
}

// A stop that fails leaves no stop-requested marker behind, so a relaunch beside
// the still-running session cannot treat it as ended.
func TestFailedWorkerStopClearsTheStopRequest(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.svc.stops = &stopRequests{}
	if _, err := r.launchAs(launcherWith("*"), "sess-live", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.DB().Exec(`UPDATE sessions SET state='running' WHERE id='sess-live'`); err != nil {
		t.Fatal(err)
	}
	r.failStops.Store(true)
	if err := r.svc.StopSession("sess-live"); err == nil {
		t.Fatal("stop should have failed")
	}
	if r.svc.stops.requested("sess-live") {
		t.Fatal("a failed stop left the stop-requested marker")
	}
	r.relaunchAs(launcherWith(launchprofile.DefaultMCPScopes[0]), "sess-beside")
	if got := r.storedScopes("sess-beside"); len(got) != 1 {
		t.Fatalf("relaunch beside a session whose stop failed took its grant: %v", got)
	}
	r.failStops.Store(false)
	if err := r.svc.StopSession("sess-beside"); err != nil {
		t.Fatal(err)
	}
	if !r.svc.stops.requested("sess-beside") {
		t.Fatal("an accepted stop must stay requested until the worker reports the exit")
	}
}
