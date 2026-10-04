package teamhost_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

var ctx = context.Background()
var errLost = errors.New("acknowledgement errLost")

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func limits() mesh.Limits {
	return mesh.Limits{MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 100, Timeout: time.Minute}
}
func definition() teams.Team {
	return teams.Team{ID: "team-one", Name: "One", Version: 1,
		Slots: []teams.Slot{
			{Name: "root", Definition: mesh.DefinitionRef{ID: "root", Revision: "r1"}, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1},
			{Name: "worker", Definition: mesh.DefinitionRef{ID: "worker", Revision: "r1"}, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1},
			{Name: "subworker", Definition: mesh.DefinitionRef{ID: "subworker", Revision: "r1"}, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1},
		},
		Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"root", "worker", "subworker"}, OwnerSlot: "root", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{
			{FromSlot: "root", Verb: teams.MaySpawn, ToSlot: "worker"}, {FromSlot: "worker", Verb: teams.MaySpawn, ToSlot: "subworker"},
			{FromSlot: "root", Verb: teams.MayAdmin, ToSlot: "worker"}, {FromSlot: "root", Verb: teams.MayAdmin, ToSlot: "subworker"}, {FromSlot: "root", Verb: teams.MayDelegate, ToSlot: "worker"},
			{FromSlot: "root", Verb: teams.MayMessage, ToSlot: "worker"}, {FromSlot: "root", Verb: teams.MaySignalPhase, ToSlot: teams.Self},
		}}, Policy: teams.Policy{Spawn: limits(), History: mesh.HistoryNone}, Routing: teams.Routing{CoordinatorSlot: "root"}}
}

// fakePorts persists its receipts across host/database reopen. Hooks execute
// after side effects and outside the lock, including a deliberately paused ack.
type fakePorts struct {
	mu                                     sync.Mutex
	enrolled                               map[mesh.URN]bool
	enrollments                            map[string]teamhost.Enrollment
	ensureRequests                         map[string]teamhost.EnrollmentRequest
	bindings                               map[mesh.URN]string
	enrollmentEnded, bindingEnded, stopped map[string]bool
	sessions                               map[string]string
	launches                               map[string]teamhost.SessionRequest
	deliveries                             map[string]teams.Delivery
	stopOrder                              []string
	hook                                   func(string, string) error
	before                                 func(string, string) error
	unfenced                               bool
}

func fake() *fakePorts {
	return &fakePorts{enrolled: map[mesh.URN]bool{}, enrollments: map[string]teamhost.Enrollment{}, ensureRequests: map[string]teamhost.EnrollmentRequest{}, bindings: map[mesh.URN]string{}, enrollmentEnded: map[string]bool{}, bindingEnded: map[string]bool{}, stopped: map[string]bool{}, sessions: map[string]string{}, launches: map[string]teamhost.SessionRequest{}, deliveries: map[string]teams.Delivery{}}
}
func (f *fakePorts) after(op, key string) error {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(op, key)
	}
	return nil
}
func (f *fakePorts) failOnce(op string) {
	var mu sync.Mutex
	seen := false
	f.mu.Lock()
	f.hook = func(actual, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		if actual == op && !seen {
			seen = true
			return errLost
		}
		return nil
	}
	f.mu.Unlock()
}
func (f *fakePorts) Ensure(_ context.Context, req teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	if f.before != nil {
		if err := f.before("ensure", req.IntentKey); err != nil {
			return teamhost.Enrollment{}, err
		}
	}
	f.mu.Lock()
	if !f.unfenced && f.enrollmentEnded[req.IntentKey] {
		f.mu.Unlock()
		return teamhost.Enrollment{}, teams.ErrDenied
	}
	if old, ok := f.ensureRequests[req.IntentKey]; ok && !reflect.DeepEqual(old, req) {
		f.mu.Unlock()
		return teamhost.Enrollment{}, teams.ErrConflict
	}
	if req.Provision.Slot.Definition.Revision != "" && req.Provision.Slot.Definition.Revision != "r1" {
		f.mu.Unlock()
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	ephemeral := req.Provision.Slot.Resolution == teams.Fresh
	if !ephemeral && !f.enrolled[req.Actor] {
		f.mu.Unlock()
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	actor := req.Actor
	if ephemeral {
		actor = mesh.URN("msg://agent/team/" + req.IntentKey)
	}
	e := teamhost.Enrollment{Actor: actor, AgentID: string(actor), Kind: mesh.ActorAgent, Ephemeral: ephemeral, SpawnCapable: true}
	f.enrolled[actor] = true
	f.enrollments[req.IntentKey] = e
	f.ensureRequests[req.IntentKey] = req
	f.mu.Unlock()
	return e, f.after("ensure", req.IntentKey)
}
func (f *fakePorts) AcquireBinding(_ context.Context, key string, actor mesh.URN) error {
	if f.before != nil {
		if err := f.before("binding", key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	if !f.unfenced && f.bindingEnded[key] {
		f.mu.Unlock()
		return teams.ErrDenied
	}
	if owner := f.bindings[actor]; !f.unfenced && owner != "" && owner != key {
		f.mu.Unlock()
		return teams.ErrProvisionFailed
	}
	f.bindings[actor] = key
	f.mu.Unlock()
	return f.after("binding", key)
}
func (f *fakePorts) ReleaseBinding(_ context.Context, key string) error {
	if f.before != nil {
		if err := f.before("release-binding", key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.bindingEnded[key] = true
	for actor, owner := range f.bindings {
		if owner == key {
			delete(f.bindings, actor)
		}
	}
	f.mu.Unlock()
	return f.after("release-binding", key)
}
func (f *fakePorts) Release(_ context.Context, key string) error {
	if f.before != nil {
		if err := f.before("release", key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.enrollmentEnded[key] = true
	f.mu.Unlock()
	return f.after("release", key)
}
func (f *fakePorts) Retire(_ context.Context, key string) error {
	if f.before != nil {
		if err := f.before("retire", key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.enrollmentEnded[key] = true
	if e, ok := f.enrollments[key]; ok && e.Ephemeral {
		delete(f.enrolled, e.Actor)
	}
	f.mu.Unlock()
	return f.after("retire", key)
}
func (f *fakePorts) Launch(_ context.Context, req teamhost.SessionRequest) (string, error) {
	if f.before != nil {
		if err := f.before("launch", req.IntentKey); err != nil {
			return "", err
		}
	}
	f.mu.Lock()
	if !f.unfenced && f.stopped[req.IntentKey] {
		f.mu.Unlock()
		return "", teams.ErrDenied
	}
	if !f.unfenced && f.bindings[req.Actor] != req.IntentKey {
		f.mu.Unlock()
		return "", teams.ErrDenied
	}
	if old, ok := f.launches[req.IntentKey]; ok && !reflect.DeepEqual(old, req) {
		f.mu.Unlock()
		return "", teams.ErrConflict
	}
	session := "session-" + req.IntentKey
	f.sessions[req.IntentKey] = session
	f.launches[req.IntentKey] = req
	f.mu.Unlock()
	return session, f.after("launch", req.IntentKey)
}
func (f *fakePorts) Stop(_ context.Context, key string) error {
	if f.before != nil {
		if err := f.before("stop", key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.stopped[key] = true
	delete(f.sessions, key)
	f.stopOrder = append(f.stopOrder, key)
	f.mu.Unlock()
	return f.after("stop", key)
}
func (f *fakePorts) Deliver(_ context.Context, d teams.Delivery) error {
	if f.before != nil {
		if err := f.before("deliver", d.IdempotencyKey); err != nil {
			return err
		}
	}
	f.mu.Lock()
	if old, ok := f.deliveries[d.IdempotencyKey]; ok {
		f.mu.Unlock()
		if !reflect.DeepEqual(old, d) {
			return teams.ErrConflict
		}
		return f.after("deliver", d.IdempotencyKey)
	}
	live := false
	for _, session := range f.sessions {
		if session == d.Recipient.SessionID {
			live = true
		}
	}
	if !live {
		f.mu.Unlock()
		return teams.ErrUnavailable
	}
	f.deliveries[d.IdempotencyKey] = d
	f.mu.Unlock()
	return f.after("deliver", d.IdempotencyKey)
}
func (f *fakePorts) Name(run string) (string, error) { return teamstore.ChannelName(run) }
func options() teamhost.Options {
	tiers := map[mesh.DefinitionRef]teamhost.TrustTier{}
	for _, slot := range definition().Slots {
		tiers[slot.Definition] = teamhost.TrustTrusted
	}
	return teamhost.Options{TrustTiers: tiers}
}
func open(t *testing.T, path string, f *fakePorts) (*store.Store, *teamstore.Store, *teamhost.Host) {
	t.Helper()
	db, err := store.Open(path)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	host, err := teamhost.New(db.DB(), storage, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, options())
	must(t, err)
	return db, storage, host
}
func launcher(s *teamstore.Store, h *teamhost.Host) *teams.Launcher {
	return &teams.Launcher{Definitions: s, Roster: s, Ledger: s, Provisioner: h, Workflows: h, Routing: h, Clock: h, IDs: h, Defaults: limits()}
}
func launch(t *testing.T, s *teamstore.Store, h *teamhost.Host) (teams.TeamRun, teams.Member) {
	t.Helper()
	must(t, s.PutDefinition(ctx, definition()))
	run, err := launcher(s, h).Launch(ctx, teams.LaunchRequest{Key: "launch", TeamID: "team-one", Version: 1})
	must(t, err)
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	if len(roster.Members) != 1 {
		t.Fatal("initial membership")
	}
	return run, roster.Members[0]
}
func childLimits() mesh.Limits { l := limits(); l.Budget = 10; return l }
func spawn(t *testing.T, s *teamstore.Store, h *teamhost.Host, run teams.TeamRun, parent teams.Member, key string) teams.Member {
	t.Helper()
	target := "worker"
	if parent.Slot == "worker" {
		target = "subworker"
	}
	m, err := teams.Spawn(ctx, definition(), s, h, h, h, run.ID, parent.Actor, target, key, childLimits(), limits())
	must(t, err)
	return m
}
func TestLauncherReopenAndExternalAcknowledgementLoss(t *testing.T) {
	for _, op := range []string{"ensure", "binding", "launch"} {
		t.Run(op, func(t *testing.T) {
			path := t.TempDir() + "/state.db"
			f := fake()
			db, s, h := open(t, path, f)
			must(t, s.PutDefinition(ctx, definition()))
			f.failOnce(op)
			request := teams.LaunchRequest{Key: "launch", TeamID: "team-one", Version: 1}
			if _, err := launcher(s, h).Launch(ctx, request); !errors.Is(err, errLost) {
				t.Fatalf("failure injection: %v", err)
			}
			must(t, db.Close())
			_, s, h = open(t, path, f)
			report, err := launcher(s, h).Reconcile(ctx, "", 8)
			must(t, err)
			if len(report.Failures) != 0 || report.Recovered != 1 {
				t.Fatalf("recovery: %+v", report)
			}
			run, err := launcher(s, h).Launch(ctx, request)
			must(t, err)
			container, err := h.GetRun(ctx, run.ID)
			must(t, err)
			if !reflect.DeepEqual(run, container) {
				t.Fatal("library run metadata changed")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.enrollments) != 1 || len(f.sessions) != 1 || len(f.bindings) != 1 {
				t.Fatal("replayed external effects created duplicates")
			}
		})
	}
}
func TestSpawnCancelCascadeAndReconcile(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f := fake()
	db, s, h := open(t, path, f)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "one")
	grandchild := spawn(t, s, h, run, worker, "two")
	f.mu.Lock()
	parent := f.launches[grandchild.Intent.IdempotencyKey].ParentSession
	f.mu.Unlock()
	if parent != worker.SessionID {
		t.Fatal("spawn port lacks retained parent session")
	}
	f.failOnce("stop")
	if err := teams.CancelMembers(ctx, definition(), s, h, run.ID, root.Actor, worker.ID, true); !errors.Is(err, errLost) {
		t.Fatalf("cancel ack loss: %v", err)
	}
	must(t, db.Close())
	_, s, h = open(t, path, f)
	must(t, teams.ReconcileMembers(ctx, s, h, run.ID))
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	for _, m := range roster.Members {
		if m.ID != root.ID && m.Status != "stopped" {
			t.Fatalf("unfinished cascade: %+v", m)
		}
	}
	f.mu.Lock()
	if f.stopOrder[0] != grandchild.Intent.IdempotencyKey {
		t.Error("parent stopped before child")
	}
	f.mu.Unlock()
	if _, err := h.Provision(ctx, *worker.Intent); !errors.Is(err, teams.ErrDenied) {
		t.Fatalf("tombstone revived: %v", err)
	}
	must(t, teams.EndRun(ctx, run.ID, s, h, h))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 0 || len(f.bindings) != 0 || len(f.enrolled) != 0 {
		t.Fatal("ephemeral run leaked resources")
	}
}
func TestSpawnProvisionRecovery(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f := fake()
	db, s, h := open(t, path, f)
	run, root := launch(t, s, h)
	f.failOnce("launch")
	if _, err := teams.Spawn(ctx, definition(), s, h, h, h, run.ID, root.Actor, "worker", "retry", childLimits(), limits()); !errors.Is(err, errLost) {
		t.Fatalf("spawn ack loss: %v", err)
	}
	must(t, db.Close())
	_, s, h = open(t, path, f)
	must(t, teams.ReconcileMembers(ctx, s, h, run.ID))
	m := spawn(t, s, h, run, root, "retry")
	if m.Status != "active" || m.Parent != root.ID {
		t.Fatal("spawn recovery changed intent")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 2 {
		t.Fatal("spawn duplicated session")
	}
}
func direct(t *testing.T, s *teamstore.Store, key string, resolution teams.Resolution, actor mesh.URN) teams.ProvisionRequest {
	t.Helper()
	team := definition()
	team.ID = "direct-" + string(resolution)
	team.Slots[1].Resolution = resolution
	if resolution == teams.Durable {
		team.Slots[1].Identity = actor
		team.Slots[1].Max = 1
		team.Slots[1].Activation = teams.Singleton
	}
	if resolution == teams.Pool {
		team.Slots[1].Identities = []mesh.URN{actor}
		team.Slots[1].Pool = "workers"
		team.Slots[1].Max = 1
	}
	must(t, s.PutDefinition(ctx, team))
	run := teams.TeamRun{ID: team.ID, TeamID: team.ID, TeamVersion: 1, Status: mesh.TaskWorking}
	_, err := s.CreateRun(ctx, run)
	must(t, err)
	return teams.ProvisionRequest{IdempotencyKey: key, RunID: run.ID, MemberID: key, Slot: team.Slots[1], Identity: actor, Limits: limits()}
}
func TestExclusiveStableBindingAndCleanupOwnership(t *testing.T) {
	for _, resolution := range []teams.Resolution{teams.Durable, teams.Pool} {
		t.Run(string(resolution), func(t *testing.T) {
			f := fake()
			_, s, h := open(t, t.TempDir()+"/state.db", f)
			actor := mesh.URN("msg://agent/test/stable")
			f.enrolled[actor] = true
			req := direct(t, s, "first", resolution, actor)
			m, err := h.Provision(ctx, req)
			must(t, err)
			rival := req
			rival.IdempotencyKey = "second"
			rival.MemberID = "second"
			if _, err := h.Provision(ctx, rival); !errors.Is(err, teams.ErrProvisionFailed) {
				t.Fatalf("duplicate binding: %v", err)
			}
			// Stub lacks Actor; a misleading caller projection is never ownership.
			must(t, h.Release(ctx, "cleanup", teams.Member{Actor: m.Actor, Intent: &rival}))
			f.mu.Lock()
			if f.bindings[actor] != "first" || f.sessions["first"] != m.SessionID {
				t.Error("stub released unrelated holder")
			}
			f.mu.Unlock()
			must(t, h.Release(ctx, "cleanup-first", m))
			again := req
			again.IdempotencyKey = "third"
			again.MemberID = "third"
			next, err := h.Provision(ctx, again)
			must(t, err)
			if next.Actor != actor || next.Ephemeral || next.SessionID == m.SessionID {
				t.Fatal("stable identity not retained with a new session")
			}
			if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrDenied) {
				t.Fatal("ended intent revived")
			}
		})
	}
}
func TestCleanupFencesPausedProvision(t *testing.T) {
	for _, op := range []string{"ensure", "binding", "launch"} {
		t.Run(op, func(t *testing.T) {
			f := fake()
			_, s, h := open(t, t.TempDir()+"/state.db", f)
			req := direct(t, s, "racing", teams.Fresh, "")
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			f.hook = func(actual, _ string) error {
				if actual == op {
					once.Do(func() { close(entered); <-release })
				}
				return nil
			}
			finished := make(chan error, 1)
			go func() { _, err := h.Provision(ctx, req); finished <- err }()
			<-entered
			must(t, h.Retire(ctx, "cancel", teams.Member{Intent: &req}))
			close(release)
			if err := <-finished; !errors.Is(err, teams.ErrDenied) {
				t.Fatalf("late success: %v", err)
			}
			if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrDenied) {
				t.Fatal("future provision not fenced")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.sessions) != 0 || len(f.bindings) != 0 || len(f.enrolled) != 0 {
				t.Fatal("racing cleanup leaked resources")
			}
		})
	}
}
func TestCleanupAcknowledgementRecovery(t *testing.T) {
	for _, op := range []string{"stop", "release-binding", "retire", "release"} {
		t.Run(op, func(t *testing.T) {
			f := fake()
			path := t.TempDir() + "/state.db"
			db, s, h := open(t, path, f)
			resolution := teams.Fresh
			actor := mesh.URN("")
			if op == "release" {
				resolution = teams.Durable
				actor = "msg://agent/test/stable"
				f.enrolled[actor] = true
			}
			req := direct(t, s, "cleanup", resolution, actor)
			m, err := h.Provision(ctx, req)
			must(t, err)
			f.failOnce(op)
			end := h.Retire
			if resolution == teams.Durable {
				end = h.Release
			}
			if err := end(ctx, "end", m); !errors.Is(err, errLost) {
				t.Fatalf("cleanup injection: %v", err)
			}
			must(t, db.Close())
			_, _, h = open(t, path, f)
			must(t, h.ReconcileIntents(ctx, 10))
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.sessions) != 0 || len(f.bindings) != 0 {
				t.Fatal("cleanup did not recover")
			}
			if f.enrolled[m.Actor] != (resolution == teams.Durable) {
				t.Fatal("cleanup changed identity lifetime")
			}
		})
	}
}
func TestWorkflowFenceAndStrictAuthority(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/state.db", f)
	must(t, s.PutDefinition(ctx, definition()))
	workflow, err := teams.CompileTeam(definition(), definition().Phases)
	must(t, err)
	must(t, h.FailWorkflow(ctx, "never-launched", "failed"))
	if _, err := h.LaunchWorkflow(ctx, "never-launched", workflow); !errors.Is(err, teams.ErrLaunchFailed) {
		t.Fatalf("workflow fence: %v", err)
	}
	run, err := h.LaunchWorkflow(ctx, "normal", workflow)
	must(t, err)
	must(t, h.FailWorkflow(ctx, "normal", "failed"))
	got, err := h.GetRun(ctx, run)
	must(t, err)
	if got.Status != mesh.TaskFailed {
		t.Fatal("workflow failure not reflected in run")
	}
	for _, mode := range []teams.AuthorityMode{teams.DevOpen, ""} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			team := definition()
			team.ID = "unsafe-" + string(mode)
			team.Authority.Mode = mode
			must(t, s.PutDefinition(ctx, team))
			if _, err := launcher(s, h).Launch(ctx, teams.LaunchRequest{Key: team.ID, TeamID: team.ID, Version: 1}); !errors.Is(err, teams.ErrLaunchFailed) {
				t.Fatalf("unsafe launch: %v", err)
			}
			workflow, err := teams.CompileTeam(team, team.Phases)
			must(t, err)
			if _, err := h.LaunchWorkflow(ctx, team.ID, workflow); !errors.Is(err, teams.ErrDenied) {
				t.Fatalf("unsafe workflow: %v", err)
			}
		})
	}
	f.mu.Lock()
	if len(f.enrollments) != 0 || len(f.sessions) != 0 {
		t.Error("unsafe definition reached a port")
	}
	f.mu.Unlock()
	// Related-metadata refusal rolls back a just-created container and group.
	runValue := teams.TeamRun{ID: "rollback", TeamID: "team-one", TeamVersion: 1, Status: mesh.TaskWorking}
	_, err = s.CreateRunAtomic(ctx, runValue, func(_ *sql.Conn) error { return teams.ErrDenied })
	if !errors.Is(err, teams.ErrDenied) {
		t.Fatal("atomic callback refusal errLost")
	}
	var count int
	must(t, db.DB().QueryRow(`SELECT count(*) FROM session_groups WHERE id='team.rollback'`).Scan(&count))
	if count != 0 {
		t.Fatal("refused workflow left container")
	}
}

func TestStableEnrollmentPinResolutionAndIdentityAdmission(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/state.db", f)
	team := definition()
	team.ID = "stable-unpinned"
	slot := &team.Slots[1]
	slot.Resolution = teams.Durable
	slot.Identity = "msg://agent/test/stable"
	slot.Definition = mesh.DefinitionRef{}
	must(t, s.PutDefinition(ctx, team))
	_, err := s.CreateRun(ctx, teams.TeamRun{ID: team.ID, TeamID: team.ID, TeamVersion: 1, Status: mesh.TaskWorking})
	must(t, err)
	f.enrolled[slot.Identity] = true
	req := teams.ProvisionRequest{IdempotencyKey: "stable", RunID: team.ID, MemberID: "stable", Slot: *slot, Identity: slot.Identity, Limits: limits()}
	m, err := h.Provision(ctx, req)
	must(t, err)
	if m.Actor != slot.Identity || m.Ephemeral {
		t.Fatal("existing enrolled identity not resolved")
	}
	req.IdempotencyKey = "unlisted"
	req.MemberID = "unlisted"
	req.Identity = "msg://agent/test/unlisted"
	f.enrolled[req.Identity] = true
	if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("undeclared enrolled actor admitted")
	}
}

func TestEndRunPreservesExternalOwnerResources(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db.sqlite", f)
	run, root := launch(t, s, h)
	missing := root
	missing.Intent = nil
	missing.SessionID = ""
	missing.Governance = teams.Owner
	if err := h.Release(ctx, run.ID, missing); err == nil {
		t.Fatal("host acquisition lost its intent")
	}
	caller := mesh.URN("msg://agent/external/operator")
	f.enrolled[caller] = true
	f.sessions["external"] = "caller-session"
	f.bindings[caller] = "external"
	must(t, s.Mutate(ctx, run.ID, func(r *teams.Roster) error {
		r.Members = append(r.Members, teams.Member{ID: "external-owner", Actor: caller, Kind: mesh.ActorAgent, Governance: teams.Owner, Status: "active"})
		return nil
	}))
	must(t, teams.EndRun(ctx, run.ID, s, h, h))
	if !f.enrolled[caller] || f.sessions["external"] != "caller-session" || f.bindings[caller] != "external" {
		t.Fatal("caller resources changed")
	}
	if len(f.sessions) != 1 || len(f.bindings) != 1 || len(f.enrolled) != 1 {
		t.Fatal("host resources leaked")
	}
}
