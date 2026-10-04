package teamruntime

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestServiceFormationAndDissolveUseRealAdaptersAndLeaveExternalOwnerAlone(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	effects := &sessionEffects{db: db}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, teamhost.Options{ActorTrust: map[mesh.URN]teamhost.TrustTier{"msg://service/local/owner": teamhost.TrustTrusted}, TrustTiers: map[mesh.DefinitionRef]teamhost.TrustTier{pin: teamhost.TrustTrusted}})
	check(t, err)
	service, err := teamsvc.New(teamsvc.Deps{Ceilings: ceilings{}, Principals: Principals{Mode: identity.Enforce, Sessions: e}, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: teamsvc.ConservativePolicy().Limits})
	check(t, err)
	team := teams.Team{ID: "formation", Name: "Formation", Version: 1, Slots: []teams.Slot{
		{Name: "owner", Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1},
		{Name: "worker", Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1},
	}, Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"owner", "worker"}, OwnerSlot: "owner", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}}, Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{{FromSlot: "owner", Verb: teams.MayMessage, ToSlot: "worker"}}}, Policy: teams.Policy{Spawn: teamsvc.ConservativePolicy().Limits, History: mesh.HistoryNone}, Routing: teams.Routing{CoordinatorSlot: "owner"}}
	owner := identity.WithPrincipal(ctx, identity.Principal{ID: "msg://service/local/owner", Kind: "service"})
	request := teamsvc.FormRequest{Key: "form", Team: team}
	result, err := service.Form(owner, request)
	check(t, err)
	replay, err := service.Form(owner, request)
	check(t, err)
	if result.Run.ID != replay.Run.ID || effects.launched != 1 {
		t.Fatal("formation replay duplicated acquisition")
	}
	roster, err := s.Snapshot(ctx, result.Run.ID)
	check(t, err)
	var worker teams.Member
	for _, m := range roster.Members {
		if m.Slot == "worker" {
			worker = m
		}
		if m.Governance == teams.Owner && m.SessionID != "" {
			t.Fatal("external owner acquired a session")
		}
	}
	if worker.Actor == "" || worker.SessionID == "" {
		t.Fatal("fresh worker not acquired")
	}
	names, err := db.ListChannelNames(ctx)
	check(t, err)
	if len(names) != 0 {
		t.Fatal("formation eagerly created run channel")
	}
	_, err = service.Dissolve(owner, teamsvc.RunRequest{Key: "end", RunID: result.Run.ID})
	check(t, err)
	row, err := db.GetSession(worker.SessionID)
	check(t, err)
	if row.State != "killed" {
		t.Fatal("worker session survived dissolve")
	}
	binding, err := reg.CurrentBinding(ctx, string(worker.Actor))
	if err == nil {
		t.Fatal("worker retained binding", binding)
	}
	profile, err := reg.Lookup(ctx, string(worker.Actor))
	check(t, err)
	if profile.Status == "active" {
		t.Fatal("fresh worker enrollment survived dissolve")
	}
}
