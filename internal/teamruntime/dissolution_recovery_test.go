package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/teamsvc"
	"time"
)

func (f *sessionEffects) TeamSessionRecoveryReady(ctx context.Context, id string) (bool, error) {
	return (&app.Service{Store: f.db}).TeamSessionRecoveryReady(ctx, id)
}

type dissolutionEffects struct {
	*sessionEffects
	readyHook func(string) (bool, error)
	stops     int
}

func (f *dissolutionEffects) StopTeamSession(ctx context.Context, id string) error {
	f.stops++
	return f.sessionEffects.StopTeamSession(ctx, id)
}
func (f *dissolutionEffects) TeamSessionRecoveryReady(ctx context.Context, id string) (bool, error) {
	if f.readyHook != nil {
		return f.readyHook(id)
	}
	return f.sessionEffects.TeamSessionRecoveryReady(ctx, id)
}

type mixedDissolution struct {
	path       string
	reconciler Reconciler
	effects    *dissolutionEffects

	db       *store.Store
	storage  *teamstore.Store
	host     *teamhost.Host
	service  *teamsvc.Service
	owner    context.Context
	run      string
	reviewer teams.Member
	roster   teams.Roster
}

func mixedDissolutionFixture(t *testing.T, adopters ...teamhost.Adopter) *mixedDissolution {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, s, reg := dbFixture(t, path)
	e := enroller(t, db, s, reg)
	effects := &dissolutionEffects{sessionEffects: &sessionEffects{db: db}}
	var adopter teamhost.Adopter
	if len(adopters) > 0 {
		adopter = adopters[0]
	}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}, Adopter: adopter}, teamhost.Options{ActorTrust: map[mesh.URN]teamhost.TrustTier{"msg://service/local/owner": teamhost.TrustTrusted}, TrustTiers: map[mesh.DefinitionRef]teamhost.TrustTier{pin: teamhost.TrustTrusted}})
	check(t, err)
	deps := teamsvc.Deps{Ceilings: ceilings{}, Principals: Principals{Mode: identity.Enforce, Sessions: e}, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: teamsvc.ConservativePolicy().Limits}
	service, err := teamsvc.New(deps)
	check(t, err)
	team := teams.Team{ID: "mixed", Name: "Mixed teardown", Version: 1, Authority: teams.Authority{Mode: teams.Strict}, Policy: teams.Policy{Spawn: teamsvc.ConservativePolicy().Limits, History: mesh.HistoryNone}, Routing: teams.Routing{CoordinatorSlot: "coordinator"}, Phases: []teams.Phase{{ID: "work", Kind: "flex", OwnerSlot: "coordinator", ActiveSlots: []string{"coordinator", "orchestrator", "worker", "reviewer"}, ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}}}
	for _, slot := range []string{"coordinator", "orchestrator", "worker", "reviewer"} {
		minimum := 1
		if slot == "coordinator" {
			minimum = 0
		}
		team.Slots = append(team.Slots, teams.Slot{Name: slot, Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: minimum, Max: 1})
	}
	owner := identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "msg://service/local/owner", Kind: "service"})
	formed, err := service.Form(owner, teamsvc.FormRequest{Key: "mixed-form", Team: team})
	check(t, err)
	roster, err := s.Snapshot(ctx, formed.Run.ID)
	check(t, err)
	f := &mixedDissolution{path: path, reconciler: Reconciler{Enroller: e, Sessions: sessions, Messenger: messages}, effects: effects, db: db, storage: s, host: host, owner: owner, run: formed.Run.ID}
	for _, m := range roster.Members {
		if m.Governance == teams.Owner {
			continue
		}
		exit := 7
		check(t, db.UpdateSessionState(m.SessionID, "failed", 0, &exit))
		if m.Slot == "reviewer" {
			f.reviewer = m
		} else {
			check(t, host.Retire(ctx, f.run, m))
		}
	}
	check(t, s.Mutate(ctx, f.run, func(r *teams.Roster) error {
		for i, m := range r.Members {
			switch m.Slot {
			case "coordinator":
				r.Members[i].Status = "releasing"
			case "reviewer":
				r.Members[i].Status = "stopping"
			default:
				r.Members[i].Status = "stopped"
			}
		}
		return nil
	}))
	// Routing has not been removed: EndRun failed before its last member ack.
	_, err = db.DB().Exec(`UPDATE team_runs SET payload=json_set(payload,'$.result','retained','$.extension','retained') WHERE run_id=?`, f.run)
	check(t, err)
	f.roster, err = s.Snapshot(ctx, f.run)
	check(t, err)
	// A new service instance must recover via durable evidence, not memory.
	f.service, err = teamsvc.New(deps)
	check(t, err)
	return f
}

func TestInterruptedDissolutionMixedRoster(t *testing.T) {
	for _, entry := range []string{"startup", "periodic", "fresh-key"} {
		t.Run(entry, func(t *testing.T) {
			f := mixedDissolutionFixture(t)
			before, err := f.db.GetSession(f.reviewer.SessionID)
			check(t, err)
			for i := 0; i < 2; i++ {
				switch entry {
				case "startup":
					check(t, f.host.ReconcileDissolvedRuns(ctx, 50))
				case "periodic":
					check(t, f.service.ReapRuns(ctx, 50))
				default:
					key := "recover"
					if i > 0 {
						key = "recover-again"
					}
					_, err = f.service.Dissolve(f.owner, teamsvc.RunRequest{Key: key, RunID: f.run})
					check(t, err)
				}
				run, err := f.host.GetRun(ctx, f.run)
				check(t, err)
				if run.Status != "dissolved" {
					t.Fatalf("not recovered: %+v", run)
				}
				roster, err := f.storage.Snapshot(ctx, f.run)
				check(t, err)
				for _, m := range roster.Members {
					want := "stopped"
					if m.Governance == teams.Owner {
						want = "released"
					}
					if m.Status != want {
						t.Fatalf("%s status=%s", m.Slot, m.Status)
					}
				}
				after, err := f.db.GetSession(f.reviewer.SessionID)
				check(t, err)
				if after.ExitCode != before.ExitCode || after.EndedAt != before.EndedAt {
					t.Fatal("cleanup overwrote observed outcome")
				}
				var result, extension string
				check(t, f.db.DB().QueryRow(`SELECT json_extract(payload,'$.result'),json_extract(payload,'$.extension') FROM team_runs WHERE run_id=?`, f.run).Scan(&result, &extension))
				if result != "retained" || extension != "retained" {
					t.Fatal("run metadata lost")
				}
			}
		})
	}
}

func TestInterruptedDissolutionKeepsLiveAndUnownedTransitions(t *testing.T) {
	for _, negative := range []string{"running", "launching", "remote", "wrong-owner", "wrong-session", "missing-receipt"} {
		t.Run(negative, func(t *testing.T) {
			f := mixedDissolutionFixture(t)
			key := f.reviewer.Intent.IdempotencyKey
			switch negative {
			case "running", "launching":
				check(t, f.db.UpdateSessionState(f.reviewer.SessionID, negative, 0, nil))
			case "remote":
				_, err := f.db.DB().Exec(`UPDATE sessions SET worker_id='worker' WHERE id=?`, f.reviewer.SessionID)
				check(t, err)
			case "wrong-owner":
				_, err := f.db.DB().Exec(`UPDATE team_port_intents SET acquired_urn='msg://agent/foreign/one' WHERE port_kind='enrollment' AND intent_key=?`, key)
				check(t, err)
			case "wrong-session":
				_, err := f.db.DB().Exec(`UPDATE session_idempotency SET key=key||'-changed' WHERE session_id=?`, f.reviewer.SessionID)
				check(t, err)
			case "missing-receipt":
				_, err := f.db.DB().Exec(`DELETE FROM team_port_intents WHERE port_kind='session' AND intent_key=?`, key)
				check(t, err)
			}
			check(t, f.service.ReapRuns(ctx, 50))
			check(t, f.host.ReconcileDissolvedRuns(ctx, 50))
			_, err := f.service.Dissolve(f.owner, teamsvc.RunRequest{Key: "cannot-recover", RunID: f.run})
			if err == nil {
				t.Fatal("unsafe fresh-key recovery")
			}
			run, err := f.host.GetRun(ctx, f.run)
			check(t, err)
			if run.Status != "working" {
				t.Fatal("unsafe terminalization")
			}
			var tombstone string
			check(t, f.db.DB().QueryRow(`SELECT tombstone FROM team_host_intents WHERE intent_key=?`, key).Scan(&tombstone))
			if tombstone != "" {
				t.Fatal("unverified recovery issued cleanup")
			}
		})
	}
}

func TestInterruptedDissolutionAtomicRecheck(t *testing.T) {
	f := mixedDissolutionFixture(t)
	check(t, f.host.Retire(ctx, f.run, f.reviewer))
	// A concurrent state report after cleanup invalidates the final proof.
	check(t, f.db.UpdateSessionState(f.reviewer.SessionID, "running", 0, nil))
	settled, err := f.storage.FinishInterruptedDissolution(ctx, f.roster)
	check(t, err)
	if settled {
		t.Fatal("committed over newer session outcome")
	}
	exit := 7
	check(t, f.db.UpdateSessionState(f.reviewer.SessionID, "failed", 0, &exit))
	check(t, f.storage.Mutate(ctx, f.run, func(r *teams.Roster) error { r.Members[0].Status = "active"; return nil }))
	if _, err = f.storage.FinishInterruptedDissolution(ctx, f.roster); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("stale roster accepted: %v", err)
	}
}

func TestInterruptedDissolutionDoesNotStopAfterLivenessRace(t *testing.T) {
	f := mixedDissolutionFixture(t)
	calls, before := 0, f.effects.stops
	f.effects.readyHook = func(id string) (bool, error) {
		calls++
		if calls == 3 {
			check(t, f.db.UpdateSessionState(id, "running", 0, nil))
			return false, nil
		}
		return true, nil
	}
	if err := f.service.ReapRuns(ctx, 50); !errors.Is(err, teamhost.ErrSessionUnavailable) {
		t.Fatalf("race hidden: %v", err)
	}
	if f.effects.stops != before {
		t.Fatal("recovery issued a stop after liveness changed")
	}
	f.effects.readyHook = nil
	if err := f.host.ReconcileIntents(ctx, 50); !errors.Is(err, teamhost.ErrSessionUnavailable) {
		t.Fatalf("retry evidence: %v", err)
	}
	if err := f.reconciler.reconcile(ctx, 50, time.Now().Add(time.Hour)); !errors.Is(err, teamhost.ErrSessionUnavailable) {
		t.Fatalf("port retry evidence: %v", err)
	}
	if f.effects.stops != before {
		t.Fatal("later retry issued a runtime stop")
	}
	var hostOnly, portOnly bool
	check(t, f.db.DB().QueryRow(`SELECT observation_only FROM team_host_intents WHERE intent_key=?`, f.reviewer.Intent.IdempotencyKey).Scan(&hostOnly))
	check(t, f.db.DB().QueryRow(`SELECT observation_only FROM team_port_intents WHERE port_kind='session' AND intent_key=?`, f.reviewer.Intent.IdempotencyKey).Scan(&portOnly))
	if !hostOnly || !portOnly {
		t.Fatal("observation fence was not durable")
	}

	run, err := f.host.GetRun(ctx, f.run)
	check(t, err)
	if run.Status != "working" {
		t.Fatal("raced recovery terminalized run")
	}
}

type dissolutionAdopter struct{ releases int }

func (*dissolutionAdopter) Verify(context.Context, teamhost.AdoptionCheck) (teamhost.Adoption, error) {
	return teamhost.Adoption{}, errors.New("unexpected verify")
}
func (a *dissolutionAdopter) Release(context.Context, string, teams.Member) error {
	a.releases++
	return nil
}

func TestInterruptedDissolutionReleasesAdoptedMembershipOnly(t *testing.T) {
	adopter := &dissolutionAdopter{}
	f := mixedDissolutionFixture(t, adopter)
	m := f.reviewer
	m.Resolution = teams.Durable
	m.Status = "releasing"
	req := *m.Intent
	req.Slot.Resolution = teams.Durable
	m.Intent = &req
	acquired := m
	acquired.Status = "active"
	raw := func(v any) []byte { b, err := json.Marshal(v); check(t, err); return b }
	_, err := f.db.DB().Exec(`UPDATE team_host_intents SET request=?,member=? WHERE intent_key=?`, raw(req), raw(acquired), req.IdempotencyKey)
	check(t, err)
	_, err = f.db.DB().Exec(`INSERT INTO team_host_adoptions(intent_key,run_id,actor,session_id,binding_generation,launch_id,definition_id,definition_revision,definition_digest,principal) VALUES(?,?,?,?,1,'launch','definition','r1','digest','principal')`, req.IdempotencyKey, f.run, m.Actor, m.SessionID)
	check(t, err)
	check(t, f.db.UpdateSessionState(m.SessionID, "running", 0, nil))
	check(t, f.storage.Mutate(ctx, f.run, func(r *teams.Roster) error {
		for i := range r.Members {
			if r.Members[i].ID == m.ID {
				r.Members[i] = m
			}
		}
		return nil
	}))
	before := f.effects.stops
	check(t, f.service.ReapRuns(ctx, 50))
	row, err := f.db.GetSession(m.SessionID)
	check(t, err)
	if row.State != "running" || f.effects.stops != before || adopter.releases != 1 {
		t.Fatal("adoption cleanup touched external session")
	}
	var ended string
	var bindingEnded bool
	check(t, f.db.DB().QueryRow(`SELECT ended,binding_ended FROM team_port_intents WHERE port_kind='enrollment' AND intent_key=?`, req.IdempotencyKey).Scan(&ended, &bindingEnded))
	if ended != "" || bindingEnded {
		t.Fatal("adoption cleanup touched external enrollment")
	}
	run, err := f.host.GetRun(ctx, f.run)
	check(t, err)
	if run.Status != "dissolved" {
		t.Fatal("adoption membership not released")
	}
}

func TestInterruptedDissolutionObservationFenceSurvivesRestart(t *testing.T) {
	for _, crash := range []string{"after-port", "before-port"} {
		t.Run(crash, func(t *testing.T) {
			f := mixedDissolutionFixture(t)
			calls := 0
			f.effects.readyHook = func(id string) (bool, error) {
				calls++
				if calls == 3 {
					check(t, f.db.UpdateSessionState(id, "running", 0, nil))
					return false, nil
				}
				return true, nil
			}
			if err := f.service.ReapRuns(ctx, 50); !errors.Is(err, teamhost.ErrSessionUnavailable) {
				t.Fatalf("race: %v", err)
			}
			if crash == "before-port" {
				_, err := f.db.DB().Exec(`UPDATE team_port_intents SET observation_only=0,ended='',state='done' WHERE port_kind='session' AND intent_key=?`, f.reviewer.Intent.IdempotencyKey)
				check(t, err)
			}
			check(t, f.db.Close())
			db, s, reg := dbFixture(t, f.path)
			e := enroller(t, db, s, reg)
			effects := &dissolutionEffects{sessionEffects: &sessionEffects{db: db}}
			sessions, err := NewSessions(db, s, effects, e)
			check(t, err)
			messages, err := NewMessenger(db.DB(), s, unusedMessages{})
			check(t, err)
			host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, teamhost.Options{Now: func() time.Time { return time.Now().Add(time.Hour) }})
			check(t, err)
			if err = host.ReconcileIntents(ctx, 50); !errors.Is(err, teamhost.ErrSessionUnavailable) {
				t.Fatalf("restart host: %v", err)
			}
			r := Reconciler{Enroller: e, Sessions: sessions, Messenger: messages}
			if err = r.reconcile(ctx, 50, time.Now().Add(2*time.Hour)); !errors.Is(err, teamhost.ErrSessionUnavailable) {
				t.Fatalf("restart port: %v", err)
			}
			row, err := db.GetSession(f.reviewer.SessionID)
			check(t, err)
			if effects.stops != 0 || row.State != "running" {
				t.Fatalf("restart stopped session: stops=%d state=%s", effects.stops, row.State)
			}
			run, err := host.GetRun(ctx, f.run)
			check(t, err)
			if run.Status != "working" {
				t.Fatal("restart terminalized live run")
			}
		})
	}
}

func remoteMixedExit(t *testing.T, f *mixedDissolution, identity bool) {
	t.Helper()
	_, err := f.db.DB().Exec(`UPDATE sessions SET state='completed',worker_id='remote-worker',pid=0,exit_code=NULL,ended_at='2026-11-20T19:45:36Z' WHERE id=?`, f.reviewer.SessionID)
	check(t, err)
	seq, _, err := f.db.InsertEvent(events.ScopeSession, f.reviewer.SessionID, events.KindSessionStateChanged, `{"from":"killed","to":"completed","exit_code":null,"reason":"worker_exit"}`)
	check(t, err)
	if identity {
		_, err = f.db.DB().Exec(`UPDATE events SET scope_worker_id='remote-worker' WHERE id=?`, seq)
		check(t, err)
	}
}

// Rebuild every adapter from the reopened database, as daemon restart does.
func reopenMixedDissolution(t *testing.T, f *mixedDissolution) {
	t.Helper()
	check(t, f.db.Close())
	db, s, reg := dbFixture(t, f.path)
	e := enroller(t, db, s, reg)
	effects := &dissolutionEffects{sessionEffects: &sessionEffects{db: db}}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, teamhost.Options{ActorTrust: map[mesh.URN]teamhost.TrustTier{"msg://service/local/owner": teamhost.TrustTrusted}, TrustTiers: map[mesh.DefinitionRef]teamhost.TrustTier{pin: teamhost.TrustTrusted}})
	check(t, err)
	service, err := teamsvc.New(teamsvc.Deps{Ceilings: ceilings{}, Principals: Principals{Mode: identity.Enforce, Sessions: e}, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: teamsvc.ConservativePolicy().Limits})
	check(t, err)
	f.db, f.storage, f.host, f.service, f.effects = db, s, host, service, effects
	f.reconciler = Reconciler{Enroller: e, Sessions: sessions, Messenger: messages}
}

func TestInterruptedDissolutionRemoteNullExit(t *testing.T) {
	for _, entry := range []string{"startup", "periodic", "fresh-key"} {
		t.Run(entry, func(t *testing.T) {
			f := mixedDissolutionFixture(t)
			remoteMixedExit(t, f, true)
			if entry == "fresh-key" {
				reopenMixedDissolution(t, f)
			}
			before := f.effects.stops
			for i := 0; i < 2; i++ {
				switch entry {
				case "startup":
					check(t, f.host.ReconcileDissolvedRuns(ctx, 50))
				case "periodic":
					check(t, f.service.ReapRuns(ctx, 50))
				default:
					_, err := f.service.Dissolve(f.owner, teamsvc.RunRequest{Key: entry + string(rune('a'+i)), RunID: f.run})
					check(t, err)
				}
			}
			run, err := f.host.GetRun(ctx, f.run)
			check(t, err)
			row, err := f.db.GetSession(f.reviewer.SessionID)
			check(t, err)
			if run.Status != "dissolved" || row.ExitCode.Valid || row.EndedAt.String != "2026-11-20T19:45:36Z" || f.effects.stops != before {
				t.Fatalf("remote outcome changed: run=%s row=%+v stops=%d", run.Status, row, f.effects.stops-before)
			}
		})
	}
}
func TestInterruptedDissolutionRemoteRequiresEvidence(t *testing.T) {
	for _, negative := range []string{"missing-identity", "wrong-worker", "later-custody"} {
		t.Run(negative, func(t *testing.T) {
			f := mixedDissolutionFixture(t)
			remoteMixedExit(t, f, negative != "missing-identity")
			before := f.effects.stops
			if negative == "wrong-worker" {
				_, err := f.db.DB().Exec(`UPDATE events SET scope_worker_id='foreign' WHERE session_id=?`, f.reviewer.SessionID)
				check(t, err)
			}
			if negative == "later-custody" {
				_, _, err := f.db.InsertEvent(events.ScopeSession, f.reviewer.SessionID, app.KindSessionKillOrdered, `{}`)
				check(t, err)
			}
			check(t, f.host.ReconcileDissolvedRuns(ctx, 50))
			check(t, f.service.ReapRuns(ctx, 50))
			if _, err := f.service.Dissolve(f.owner, teamsvc.RunRequest{Key: "remote-no-proof", RunID: f.run}); err == nil {
				t.Fatal("unconfirmed remote recovered")
			}
			run, err := f.host.GetRun(ctx, f.run)
			check(t, err)
			if run.Status != "working" || f.effects.stops != before {
				t.Fatal("unsafe remote teardown")
			}
		})
	}
}
func TestInterruptedDissolutionRemoteCustodyInvalidatesAck(t *testing.T) {
	f := mixedDissolutionFixture(t)
	remoteMixedExit(t, f, true)
	check(t, f.host.Retire(teamhost.WithDissolutionRecovery(ctx), f.run, f.reviewer))
	_, _, err := f.db.InsertEvent(events.ScopeSession, f.reviewer.SessionID, app.KindSessionKillOrdered, `{}`)
	check(t, err)
	settled, err := f.storage.FinishInterruptedDissolution(ctx, f.roster)
	check(t, err)
	if settled {
		t.Fatal("new custody accepted after acknowledgment")
	}
}
