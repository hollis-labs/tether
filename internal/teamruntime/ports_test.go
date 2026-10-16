package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/agentdef"
	"github.com/hollis-labs/libs/mesh/core/teams"
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
	timeouts               []time.Duration
}

// CreateTeamSessionWithLimits records the enforced spawn timeout. Like a service
// without context support, it refuses a sealed boot context.
func (f *sessionEffects) CreateTeamSessionWithLimits(ctx context.Context, key, launchID string, boot app.TeamBootContext, d time.Duration) (*app.Launched, error) {
	if boot != (app.TeamBootContext{}) {
		return nil, teamhost.ErrSessionUnavailable
	}
	f.mu.Lock()
	f.timeouts = append(f.timeouts, d)
	f.mu.Unlock()
	return f.CreateTeamSession(ctx, key, launchID)
}

func (f *sessionEffects) CreateSessionWithInput(in app.CreateSessionInput) (*app.Launched, error) {
	if f.beforeCreate != nil {
		f.beforeCreate()
	}
	record, err := f.db.GetSessionIdempotency(store.IdempotencyPrincipalTeam, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	id := "session-" + in.IdempotencyKey
	if record == nil {
		err = f.db.CreateSessionKeyed(store.SessionRow{ID: id, LaunchID: in.LaunchID, State: "created"}, &launch.Plan{LaunchID: in.LaunchID}, &store.SessionIdempotency{PrincipalID: store.IdempotencyPrincipalTeam, Key: in.IdempotencyKey, Operation: store.IdempotencyOpCreate, RequestDigest: "fixture"})
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
	record, err := db.GetSessionIdempotency(store.IdempotencyPrincipalTeam, testSessionKey(t, port, req.IntentKey))
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

// internalEffects is a session service that, like app.Service, can start a
// session bounded by an origin session.
type internalEffects struct {
	*sessionEffects
	origin []app.InternalLaunch
}

func (f *internalEffects) LaunchInternal(ctx context.Context, id string, o app.InternalLaunch) (*app.Launched, error) {
	f.origin = append(f.origin, o)
	return f.LaunchSessionWithContext(ctx, id)
}

// The team host starts a spawned member through LaunchInternal with its parent
// session as the origin, so the member cannot hold more than its parent.
func TestTeamMemberLaunchIsBoundedByItsParentSession(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	req := fresh("bounded")
	actor, err := e.Ensure(ctx, req)
	check(t, err)
	check(t, e.AcquireBinding(ctx, "bounded", actor.Actor))
	effects := &internalEffects{sessionEffects: &sessionEffects{db: db}}
	port, err := NewSessions(db, s, effects, e)
	check(t, err)
	_, err = port.Launch(ctx, teamhost.SessionRequest{IntentKey: "bounded", Actor: actor.Actor, ParentSession: "the-parent", Provision: req.Provision})
	check(t, err)
	if len(effects.origin) != 1 || effects.origin[0] != (app.InternalLaunch{Kind: app.OriginTeam, OriginSessionID: "the-parent"}) {
		t.Fatalf("launch origin %+v", effects.origin)
	}
}

func operatorAs(scopes ...string) context.Context {
	return identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: scopes})
}

// The launch under a verified caller records its grant (first write wins);
// recovery and reconcile launches, which have no caller, reuse it.
func TestTeamLaunchGrantIsRecordedOnceAndReusedWithoutACaller(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	req := fresh("granted")
	actor, err := e.Ensure(ctx, req)
	check(t, err)
	check(t, e.AcquireBinding(ctx, "granted", actor.Actor))
	effects := &internalEffects{sessionEffects: &sessionEffects{db: db}}
	port, err := NewSessions(db, s, effects, e)
	check(t, err)
	in := teamhost.SessionRequest{IntentKey: "granted", Actor: actor.Actor, Provision: req.Provision}
	// First launch, under the verb's verified caller.
	if _, err = port.Launch(operatorAs("message.write", "registry.write"), in); err != nil {
		t.Fatal(err)
	}
	if len(effects.origin) != 1 || effects.origin[0].Grant == nil || effects.origin[0].Grant.Principal != identity.OperatorID || !slices.Equal(effects.origin[0].Grant.Scopes, []string{"message.write", "registry.write"}) {
		t.Fatalf("first launch origin: %+v", effects.origin)
	}
	// Recovery: no caller. The recorded grant is what bounds it.
	got, err := port.launchGrant(ctx, "granted")
	if err != nil || got == nil || got.Principal != identity.OperatorID || !slices.Equal(got.Scopes, []string{"message.write", "registry.write"}) {
		t.Fatalf("grant without a caller: %+v %v", got, err)
	}
	// First write wins: a later caller with other scopes does not replace it.
	other := identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "msg://agent/other/x", Kind: "agent", Scopes: []string{"*"}})
	got, err = port.launchGrant(other, "granted")
	if err != nil || got.Principal != identity.OperatorID || slices.Contains(got.Scopes, "*") {
		t.Fatalf("the recorded grant was replaced: %+v %v", got, err)
	}
	// An intent with neither a caller nor a record has no grant.
	if got, err = port.launchGrant(ctx, "never-launched"); err != nil || got != nil {
		t.Fatalf("grant for an unknown intent: %+v %v", got, err)
	}
	// An unverified caller records nothing.
	unverified := identity.WithPrincipal(ctx, identity.Principal{Verified: false, ID: "msg://agent/claimed/x", Kind: "agent", Scopes: []string{"*"}})
	if got, err = port.launchGrant(unverified, "claimed-only"); err != nil || got != nil {
		t.Fatalf("an unverified caller recorded a grant: %+v %v", got, err)
	}
}

// Releasing a refused launch explains itself in the log.
func TestRefusedTeamLaunchIsLoggedWithItsReason(t *testing.T) {
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })
	logRefusedLaunch("intent-1", nil, "", fmt.Errorf("%w: identity is enforce", app.ErrLaunchRefused))
	if !strings.Contains(logs.String(), "intent-1") || !strings.Contains(logs.String(), "no recorded launch grant") {
		t.Fatalf("log: %q", logs.String())
	}
	logs.Reset()
	logRefusedLaunch("intent-2", nil, "", errors.New("something else"))
	if logs.Len() != 0 {
		t.Fatalf("an unrelated error was logged as a refusal: %q", logs.String())
	}
}

// The runbook's read-only pre-flip check, run against the real schema: it counts
// root team sessions that would be refused as unattributed under global enforce.
func TestRunbookCheckCountsRecoveryEligibleTeamSessionsWithNoGrant(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	_, _ = s, reg
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := db.DB().Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	intent := func(key, parent, ended string) {
		exec(`INSERT INTO team_port_intents(port_kind,intent_key,request,state,ended) VALUES('session',?,?,'done',?)`, key, `{"IntentKey":"`+key+`","ParentSession":"`+parent+`"}`, ended)
	}
	intent("root-no-grant", "", "")
	intent("root-granted", "", "")
	exec(`INSERT INTO team_launch_grants(intent_key,principal_id,scopes_json,recorded_at) VALUES('root-granted','msg://user/local/operator','["*"]','now')`)
	intent("spawned", "parent-session", "") // bounded by its parent's grant, not counted
	intent("root-ended", "", "stop")        // stopped, not recovery-eligible
	var n int
	if err := db.DB().QueryRow(runbookNoGrantCheck).Scan(&n); err != nil || n != 1 {
		t.Fatalf("check counted %d (%v), want 1", n, err)
	}
}

// runbookNoGrantCheck is the query printed in docs/identity-enforce-runbook.md.
const runbookNoGrantCheck = `SELECT COUNT(*) FROM team_port_intents i
 WHERE i.port_kind = 'session' AND i.ended = '' AND i.state IN ('pending','done')
   AND COALESCE(json_extract(i.request, '$.ParentSession'), '') = ''
   AND NOT EXISTS (SELECT 1 FROM team_launch_grants g WHERE g.intent_key = i.intent_key)`
