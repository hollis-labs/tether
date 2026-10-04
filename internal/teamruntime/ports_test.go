package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/agentdef"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

var ctx = context.Background()
var pin = mesh.DefinitionRef{ID: "worker", Revision: "r1", Digest: "verified-digest"}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type verifiedDefinitions struct {
	fail   error
	before func()
}

func (d *verifiedDefinitions) Load(_ context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	if d.before != nil {
		d.before()
	}
	if d.fail != nil {
		return definitionresolve.VerifiedDefinition{}, d.fail
	}
	if p != pin {
		return definitionresolve.VerifiedDefinition{}, definitionresolve.ErrPinMismatch
	}
	return definitionresolve.VerifiedDefinition{Pin: pin, Definition: &agentdef.Definition{Name: "Worker", Capabilities: []agentdef.Capability{{ID: mesh.SpawnCapabilityURI}}}}, nil
}
func dbFixture(t *testing.T, path string) (*store.Store, *teamstore.Store, *registry.Service) {
	t.Helper()
	db, err := store.Open(path)
	check(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s, err := teamstore.New(db.DB(), teamstore.Options{})
	check(t, err)
	return db, s, registry.NewService(registry.NewStorage(db.DB()))
}
func enroller(t *testing.T, db *store.Store, s *teamstore.Store, reg *registry.Service) *LegacyEnroller {
	t.Helper()
	e, err := NewLegacyEnroller(db.DB(), s, reg, &verifiedDefinitions{}, map[mesh.DefinitionRef]LaunchTarget{pin: {LaunchID: "worker-launch"}})
	check(t, err)
	return e
}
func fresh(key string) teamhost.EnrollmentRequest {
	return teamhost.EnrollmentRequest{IntentKey: key, Provision: teams.ProvisionRequest{IdempotencyKey: key, MemberID: key, Slot: teams.Slot{Name: "worker", Definition: pin, Resolution: teams.Fresh}}}
}
func TestLegacyFreshLostAckReopenAndRetirementFences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, s, reg := dbFixture(t, path)
	e := enroller(t, db, s, reg)
	req := fresh("lost")
	_, err := db.DB().Exec(`CREATE TRIGGER fail_receipt BEFORE UPDATE OF payload ON team_port_intents BEGIN SELECT RAISE(ABORT,'lost ack'); END`)
	check(t, err)
	if _, err = e.Ensure(ctx, req); err == nil {
		t.Fatal("injected post-registration failure ignored")
	}
	profile, err := reg.LookupBy(ctx, registry.KindAgent, testNonce(t, e, req.IntentKey), enrollmentSubstrate)
	check(t, err)
	if profile.URN == "" || profile.Status != registry.StatusActive {
		t.Fatal("completion failure retired acquisition", profile)
	}
	receipt, err := e.read(ctx, "enrollment", req.IntentKey)
	check(t, err)
	if receipt.state != "pending" || receipt.acquired != profile.URN {
		t.Fatal("completion failure discarded ownership", receipt)
	}
	_, err = db.DB().Exec(`DROP TRIGGER fail_receipt`)
	check(t, err)
	check(t, db.Close())
	db, s, reg = dbFixture(t, path)
	e = enroller(t, db, s, reg)
	value, err := e.Ensure(ctx, req)
	check(t, err)
	if string(value.Actor) != profile.URN || !value.Ephemeral || !value.SpawnCapable {
		t.Fatal("fresh retry reminted identity", value)
	}
	changed := req
	changed.Provision.MemberID = "other"
	numeric := fresh("large-number")
	numeric.Provision.Limits.MaxRounds = 9007199254740992
	_, err = e.Ensure(ctx, numeric)
	check(t, err)
	numeric.Provision.Limits.MaxRounds++
	if _, err = e.Ensure(ctx, numeric); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("integer request precision collapsed", err)
	}
	if _, err = e.Ensure(ctx, changed); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("changed intent accepted", err)
	}
	check(t, e.Retire(ctx, req.IntentKey))
	check(t, e.Retire(ctx, req.IntentKey))
	if _, err = e.Ensure(ctx, req); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("retirement resurrected key", err)
	}
	profile, err = reg.LookupBy(ctx, registry.KindAgent, testNonce(t, e, req.IntentKey), enrollmentSubstrate)
	check(t, err)
	if profile.Status == registry.StatusActive {
		t.Fatal("ephemeral registry enrollment survived retirement")
	}
	check(t, e.Retire(ctx, "never-started"))
	if _, err = e.Ensure(ctx, fresh("never-started")); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("stub retirement did not fence", err)
	}
}
func TestLegacyStableIdentityExclusiveBindingAndOwnedRelease(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	raw, err := json.Marshal(pin)
	check(t, err)
	profile, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Stable", Props: map[string]string{"team_definition": string(raw)}}, "fixture", "stable")
	check(t, err)
	req := fresh("one")
	req.Actor = mesh.URN(profile.URN)
	req.Provision.Identity = req.Actor
	req.Provision.Slot.Resolution = teams.Pool
	first, err := e.Ensure(ctx, req)
	check(t, err)
	if first.Actor != req.Actor || first.Ephemeral {
		t.Fatal("stable actor substituted")
	}
	check(t, e.AcquireBinding(ctx, req.IntentKey, req.Actor))
	check(t, e.AcquireBinding(ctx, req.IntentKey, req.Actor))
	rival := req
	rival.IntentKey = "two"
	rival.Provision.IdempotencyKey = "two"
	_, err = e.Ensure(ctx, rival)
	check(t, err)
	if err = e.AcquireBinding(ctx, rival.IntentKey, rival.Actor); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("live actor shared", err)
	}
	check(t, e.ReleaseBinding(ctx, rival.IntentKey))
	bound, err := reg.CurrentBinding(ctx, profile.URN)
	check(t, err)
	if bound.AttemptID != testBindingSecret(t, e, req.IntentKey) {
		t.Fatal("rival release revoked owner")
	}
	check(t, e.ReleaseBinding(ctx, req.IntentKey))
	if err = e.AcquireBinding(ctx, req.IntentKey, req.Actor); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("released key reacquired", err)
	}
	retained, err := reg.ListBindingsForTarget(ctx, profile.URN)
	check(t, err)
	if len(retained) != 1 {
		t.Fatal("released key took a new binding before compensating", retained)
	}
	check(t, e.Retire(ctx, req.IntentKey))
	profile, err = reg.Lookup(ctx, profile.URN)
	check(t, err)
	if profile.Status != registry.StatusActive {
		t.Fatal("retire removed stable caller enrollment")
	}
}

// Session effects use the real session/idempotency store. This fake has NO port
// tombstone or fencing logic, so it cannot mask adapter guards.
type sessionEffects struct {
	db                     *store.Store
	mu                     sync.Mutex
	createFault, stopFault error
	launched               int
	beforeCreate           func()
}

func (f *sessionEffects) CreateSessionWithInput(in app.CreateSessionInput) (*app.Launched, error) {
	if f.beforeCreate != nil {
		f.beforeCreate()
	}
	record, err := f.db.GetSessionIdempotency(in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	id := "session-" + in.IdempotencyKey
	if record == nil {
		err = f.db.CreateSessionKeyed(store.SessionRow{ID: id, LaunchID: in.LaunchID, State: "created"}, &launch.Plan{LaunchID: in.LaunchID}, &store.SessionIdempotency{Key: in.IdempotencyKey, Operation: store.IdempotencyOpCreate, RequestDigest: "fixture"})
	} else {
		id = record.SessionID
	}
	if err != nil {
		return nil, err
	}
	if f.createFault != nil {
		return nil, f.createFault
	}
	return &app.Launched{SessionID: id}, nil
}
func (f *sessionEffects) LaunchSessionWithContext(_ context.Context, id string) (*app.Launched, error) {
	row, err := f.db.GetSession(id)
	if err != nil {
		return nil, err
	}
	if row.State == "created" {
		f.mu.Lock()
		f.launched++
		f.mu.Unlock()
		err = f.db.UpdateSessionState(id, "running", 0, nil)
	}
	return &app.Launched{SessionID: id}, err
}
func (f *sessionEffects) LinkTeamSession(ctx context.Context, id, key, parent string) error {
	return (&app.Service{Store: f.db}).LinkTeamSession(ctx, id, key, parent)
}
func (f *sessionEffects) StopSession(id string) error {
	if f.stopFault != nil {
		return f.stopFault
	}
	return f.db.UpdateSessionState(id, "killed", 0, nil)
}
func (f *sessionEffects) WaitSession(context.Context, string) (int, error) { return 0, nil }
func sessionFixture(t *testing.T, db *store.Store, s *teamstore.Store, reg *registry.Service, key string) (*Sessions, *LegacyEnroller, *sessionEffects, teamhost.SessionRequest) {
	e := enroller(t, db, s, reg)
	req := fresh(key)
	actor, err := e.Ensure(ctx, req)
	check(t, err)
	check(t, e.AcquireBinding(ctx, key, actor.Actor))
	effects := &sessionEffects{db: db}
	port, err := NewSessions(db, s, effects, e)
	check(t, err)
	return port, e, effects, teamhost.SessionRequest{IntentKey: key, Actor: actor.Actor, ParentSession: "parent", Provision: req.Provision}
}
func TestSessionLostCreateAckStopAndLateLaunchFence(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, _, effects, req := sessionFixture(t, db, s, reg, "lost-create")
	effects.createFault = errors.New("lost create ack")
	if _, err := port.Launch(ctx, req); err == nil {
		t.Fatal("lost create ack ignored")
	}
	record, err := db.GetSessionIdempotency(testSessionKey(t, port, req.IntentKey))
	check(t, err)
	if record == nil {
		t.Fatal("create effect missing")
	}
	check(t, port.Stop(ctx, req.IntentKey))
	effects.createFault = nil
	if _, err = port.Launch(ctx, req); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("late launch recreated stopped intent", err)
	}
	row, err := db.GetSession(record.SessionID)
	check(t, err)
	if row.State != "killed" || effects.launched != 0 {
		t.Fatal("stop missed unacknowledged create")
	}
	check(t, port.Stop(ctx, "stub"))
	if _, err = port.Launch(ctx, teamhost.SessionRequest{IntentKey: "stub"}); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("stub Stop did not fence", err)
	}
}
func TestSessionRetryRetainsIdentityParentAndWaitsForStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, s, reg := dbFixture(t, path)
	port, e, effects, req := sessionFixture(t, db, s, reg, "live")
	id, err := port.Launch(ctx, req)
	check(t, err)
	again, err := port.Launch(ctx, req)
	check(t, err)
	if id != again || effects.launched != 1 {
		t.Fatal("session retry relaunched")
	}
	row, err := db.GetSession(id)
	check(t, err)
	if row.ParentSessionID.String != req.ParentSession {
		t.Fatal("parent not retained")
	}
	binding, err := reg.CurrentBinding(ctx, string(req.Actor))
	check(t, err)
	if binding.SessionID != id {
		t.Fatal("binding targets reservation instead of session")
	}
	changed := req
	changed.ParentSession = "another"
	if _, err = port.Launch(ctx, changed); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("changed launch accepted", err)
	}
	effects.stopFault = errors.New("stop outage")
	if err = port.Stop(ctx, req.IntentKey); err == nil {
		t.Fatal("stop failure swallowed")
	}
	binding, err = reg.CurrentBinding(ctx, string(req.Actor))
	check(t, err)
	if binding.SessionID != id {
		t.Fatal("failed Stop released binding")
	}
	effects.stopFault = nil
	check(t, port.Stop(ctx, req.IntentKey))
	check(t, e.ReleaseBinding(ctx, req.IntentKey))
	if _, err = port.Launch(ctx, req); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("Stop fence lost", err)
	}
}

func (f *sessionEffects) CreateTeamSession(_ context.Context, key, launchID string) (*app.Launched, error) {
	return f.CreateSessionWithInput(app.CreateSessionInput{LaunchID: launchID, IdempotencyKey: key})
}
func (f *sessionEffects) StopTeamSession(ctx context.Context, id string) error {
	row, err := f.db.GetSession(id)
	if err != nil {
		return err
	}
	if session.State(row.State).Terminal() {
		return nil
	}
	if row.State == "created" {
		return f.db.UpdateSessionState(id, "killed", 0, nil)
	}
	if err = f.StopSession(id); err != nil {
		return err
	}
	_, err = f.WaitSession(ctx, id)
	return err
}

func testNonce(t *testing.T, e *LegacyEnroller, key string) string {
	t.Helper()
	r, err := e.read(ctx, "enrollment", key)
	check(t, err)
	return r.nonce
}
func testSessionKey(t *testing.T, p *Sessions, key string) string {
	t.Helper()
	r, err := p.read(ctx, "session", key)
	check(t, err)
	return sessionKey(r.nonce)
}

func testBindingSecret(t *testing.T, e *LegacyEnroller, key string) string {
	t.Helper()
	r, err := e.read(ctx, "enrollment", key)
	check(t, err)
	return r.bindingSecret
}
