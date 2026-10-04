package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestStopCommitsFenceDuringCreateAndCompensatesLateEffect(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, _, effects, req := sessionFixture(t, db, s, reg, "race")
	entered, proceed := make(chan struct{}), make(chan struct{})
	effects.beforeCreate = func() { close(entered); <-proceed }
	launch := make(chan error, 1)
	go func() { _, err := port.Launch(ctx, req); launch <- err }()
	<-entered
	r, err := port.read(ctx, "session", req.IntentKey)
	check(t, err)
	if len(r.request) == 0 || r.state != "pending" {
		t.Fatal("external create started before durable intent")
	}
	stop := make(chan error, 1)
	go func() { stop <- port.Stop(ctx, req.IntentKey) }()
	deadline := time.Now().Add(time.Second * 3)
	for {
		r, err = port.read(ctx, "session", req.IntentKey)
		check(t, err)
		if r.ended == "stop" {
			break
		}
		if time.Now().After(deadline) {
			close(proceed)
			t.Fatal("Stop waited for the effect before committing its fence")
		}
		time.Sleep(time.Millisecond)
	}
	close(proceed)
	if err = <-launch; !errors.Is(err, teams.ErrDenied) {
		t.Fatal("late create accepted", err)
	}
	check(t, <-stop)
	acquired, err := db.GetSessionIdempotency(testSessionKey(t, port, req.IntentKey))
	check(t, err)
	row, err := db.GetSession(acquired.SessionID)
	check(t, err)
	if row.State != "killed" || effects.launched != 0 {
		t.Fatal("late effect escaped cleanup", row.State, effects.launched)
	}
}
func TestSessionBindingUpdateRefusesConcurrentRelease(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, e, effects, req := sessionFixture(t, db, s, reg, "binding-race")
	effects.beforeCreate = func() { check(t, e.end(ctx, "enrollment", req.IntentKey, "", true)) }
	if _, err := port.Launch(ctx, req); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("binding tombstone ignored", err)
	}
	if effects.launched != 0 {
		t.Fatal("started after binding ended")
	}
	binding, err := reg.CurrentBinding(ctx, string(req.Actor))
	check(t, err)
	if binding.SessionID != "team-intent:"+testBindingSecret(t, e, req.IntentKey) {
		t.Fatal("late call mutated ended binding", binding.SessionID)
	}
	check(t, port.Stop(ctx, req.IntentKey))
	check(t, e.ReleaseBinding(ctx, req.IntentKey))
}
func TestReconcileCursorHostTombstonesAndSessionOrphans(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	effects := &sessionEffects{db: db}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r := Reconciler{e, sessions, messages}
	bad := fresh("first-bad")
	bad.Provision.Slot.Definition.Revision = "missing"
	_, err = e.reserve(ctx, "enrollment", bad.IntentKey, bad)
	check(t, err)
	_, err = e.reserve(ctx, "enrollment", "later-good", fresh("later-good"))
	check(t, err)
	if err = r.Reconcile(ctx, 1); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("permanent pin refusal lost", err)
	}
	check(t, r.Reconcile(ctx, 1))
	saved, err := e.read(ctx, "enrollment", "later-good")
	check(t, err)
	if saved.state != "done" {
		t.Fatal("failing head starved later receipt")
	}
	// A host tombstone must prevent recovery from acquiring an unused identity.
	raw, err := json.Marshal(fresh("host-ended"))
	check(t, err)
	_, err = e.reserve(ctx, "enrollment", "host-ended", fresh("host-ended"))
	check(t, err)
	_, err = db.DB().Exec(`INSERT INTO team_host_intents(intent_key,request,tombstone) VALUES(?,?,'retire')`, "host-ended", raw)
	check(t, err)
	check(t, r.Reconcile(ctx, 20)) // Permanent refusals are terminal; later pages must not retry them.
	if _, err = reg.LookupBy(ctx, registry.KindAgent, "host-ended", enrollmentSubstrate); !errors.Is(err, registry.ErrNotFound) {
		t.Fatal("host-ended acquisition recreated", err)
	}
	saved, err = e.read(ctx, "enrollment", "host-ended")
	check(t, err)
	if saved.ended != "retire" || saved.state != "cleaned" {
		t.Fatal("host tombstone not retained", saved)
	}
	_, err = effects.CreateTeamSession(ctx, sessionKey("orphan"), "worker-launch")
	check(t, err)
	check(t, r.Reconcile(ctx, 20))
	acquired, err := db.GetSessionIdempotency(sessionKey("orphan"))
	check(t, err)
	row, err := db.GetSession(acquired.SessionID)
	check(t, err)
	if row.State != "created" {
		t.Fatal("foreign session mutated", row.State)
	}
	if _, err = sessions.read(ctx, "session", "orphan"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("foreign session acquired a receipt", err)
	}
}
func TestReconcileNeverAdoptsMailboxTransportReceipts(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r := Reconciler{e, sessions, messages}
	accepted := delivery("accepted")
	accepted.Recipient = teams.Member{Actor: "msg://user/local/owner", Kind: mesh.ActorUser}
	service := &app.Service{Store: db}
	_, err = messages.reserve(ctx, "delivery", accepted.IdempotencyKey, accepted)
	check(t, err)
	check(t, service.QueueTeamDelivery(ctx, accepted))
	forged := accepted
	forged.IdempotencyKey = "forged"
	raw, err := json.Marshal(forged)
	check(t, err)
	_, err = db.MessagingStore().Send(ctx, messaging.Envelope{From: messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "sender"}, To: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "owner"}, Kind: messaging.MsgKindNotice, Payload: []byte(`{"body":"hello"}`), ContentType: "application/json", Metadata: map[string]string{"team_delivery_key": forged.IdempotencyKey, "team_delivery_request": string(raw)}})
	check(t, err)
	check(t, r.Reconcile(ctx, 20))
	saved, err := e.read(ctx, "delivery", "accepted")
	check(t, err)
	if saved.state != "pending" {
		t.Fatal("recovery bypassed host delivery", saved)
	}
	if _, err = e.read(ctx, "delivery", "forged"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("metadata assertion forged receipt", err)
	}
}
func TestVerifiedOrdinarySessionRequiresExactCurrentBinding(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	actor := registry.LogicalAgentBindingTarget("ordinary")
	check(t, db.CreateSession(store.SessionRow{ID: "ordinary-session", LogicalAgentID: "ordinary", State: "running"}, nil))
	_, err := reg.LeaseBinding(ctx, actor, "ordinary-session", "daemon", "ordinary", nil, registry.VisibilityTetherHosted, 0)
	check(t, err)
	p := Principals{Mode: identity.Enforce, Sessions: e}
	caller := identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://session/local/ordinary-session", Kind: "session", SessionID: "ordinary-session"})
	out, err := p.ResolvePrincipal(caller)
	check(t, err)
	if string(out.ID) != actor || !out.Verified {
		t.Fatal("ordinary authenticated agent lost", out)
	}
	_, err = reg.LeaseBinding(ctx, actor, "successor", "daemon", "successor", nil, registry.VisibilityTetherHosted, 0)
	check(t, err)
	if _, err = p.ResolvePrincipal(caller); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("stale session principal borrowed successor binding")
	}
}

func TestRetireCommitsFenceDuringRegistrationAndCleansLateEnrollment(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	entered, proceed := make(chan struct{}), make(chan struct{})
	defs := &verifiedDefinitions{before: func() { close(entered); <-proceed }}
	e, err := NewLegacyEnroller(db.DB(), s, reg, defs, map[mesh.DefinitionRef]LaunchTarget{pin: {LaunchID: "worker-launch"}})
	check(t, err)
	req := fresh("registration-race")
	ensure := make(chan error, 1)
	go func() { _, err := e.Ensure(ctx, req); ensure <- err }()
	<-entered
	saved, err := e.read(ctx, "enrollment", req.IntentKey)
	check(t, err)
	if len(saved.request) == 0 || saved.state != "pending" {
		t.Fatal("registration started without committed intent")
	}
	retire := make(chan error, 1)
	go func() { retire <- e.Retire(ctx, req.IntentKey) }()
	deadline := time.Now().Add(time.Second * 3)
	for {
		saved, err = e.read(ctx, "enrollment", req.IntentKey)
		check(t, err)
		if saved.ended == "retire" {
			break
		}
		if time.Now().After(deadline) {
			close(proceed)
			t.Fatal("retirement did not commit fence before cleanup")
		}
		time.Sleep(time.Millisecond)
	}
	close(proceed)
	if err = <-ensure; !errors.Is(err, teams.ErrDenied) {
		t.Fatal("late registration accepted", err)
	}
	check(t, <-retire)
	profile, err := reg.LookupBy(ctx, registry.KindAgent, testNonce(t, e, req.IntentKey), enrollmentSubstrate)
	check(t, err)
	if profile.Status == registry.StatusActive {
		t.Fatal("late enrollment escaped retirement")
	}
	saved, err = e.read(ctx, "enrollment", req.IntentKey)
	check(t, err)
	if saved.state != "cleaned" || len(saved.payload) > 0 {
		t.Fatal("late completion wrote after tombstone", saved)
	}
}
func TestPermanentEnrollmentRefusalsNeverMintAnIdentity(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e, err := NewLegacyEnroller(db.DB(), s, reg, &verifiedDefinitions{}, nil)
	check(t, err)
	if _, err = e.Ensure(ctx, fresh("no-target")); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("target omission allowed", err)
	}
	if _, err = reg.LookupBy(ctx, registry.KindAgent, "no-target", enrollmentSubstrate); !errors.Is(err, registry.ErrNotFound) {
		t.Fatal("refused target created identity", err)
	}
	e = enroller(t, db, s, reg)
	req := fresh("chosen-identity")
	req.Actor = "msg://agent/local/chosen"
	if _, err = e.Ensure(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("fresh caller chose URN", err)
	}
	req = fresh("invalid-pin")
	req.Provision.Slot.Definition.Revision = "changed"
	if _, err = e.Ensure(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("permanent pin mismatch holds quota", err)
	}
}

func TestAcquireBindingCompensatesFenceAfterExternalLease(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	value, err := e.Ensure(ctx, fresh("lease-race"))
	check(t, err)
	_, err = db.DB().Exec(`CREATE TRIGGER end_during_binding AFTER INSERT ON runtime_bindings BEGIN UPDATE team_port_intents SET binding_ended=1 WHERE port_kind='enrollment' AND binding_secret=NEW.attempt_id; END`)
	check(t, err)
	if err = e.AcquireBinding(ctx, "lease-race", value.Actor); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("late binding accepted", err)
	}
	if _, err = reg.CurrentBinding(ctx, string(value.Actor)); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatal("late binding escaped compensation", err)
	}
}

func TestStableRetirementDoesNotAdoptUnrecordedFreshEnrollment(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	raw, err := json.Marshal(pin)
	check(t, err)
	stable, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Stable", Props: map[string]string{"team_definition": string(raw)}}, "caller", "stable")
	check(t, err)
	req := fresh("stable-receipt")
	req.Actor = mesh.URN(stable.URN)
	req.Provision.Identity = req.Actor
	req.Provision.Slot.Resolution = teams.Durable
	_, err = e.Ensure(ctx, req)
	check(t, err)
	// Even a namespaced entry cannot be adopted by cleanup when this receipt
	// records verification of a stable actor, with no ephemeral acquisition.
	unrecorded, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Unrecorded"}, enrollmentSubstrate, req.IntentKey)
	check(t, err)
	check(t, e.Retire(ctx, req.IntentKey))
	for _, urn := range []string{stable.URN, unrecorded.URN} {
		profile, err := reg.Lookup(ctx, urn)
		check(t, err)
		if profile.Status != registry.StatusActive {
			t.Fatal("retirement adopted an enrollment it did not acquire", urn)
		}
	}
}
