package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	messageDelivery "github.com/hollis-labs/substrate/mesh/messaging/delivery"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestPredictableNamespacesNeverConferOwnership(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	raw, _ := json.Marshal(pin)
	victim, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Foreign", Props: map[string]string{"team_definition": string(raw)}}, enrollmentSubstrate, "predicted")
	check(t, err)
	decoy, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Merged decoy"}, enrollmentSubstrate, "merged-decoy")
	check(t, err)
	_, err = reg.Merge(ctx, decoy.URN, victim.URN)
	check(t, err)
	_, err = reg.LeaseBinding(ctx, victim.URN, "foreign", "team", "predicted", nil, registry.VisibilityTetherHosted, 0)
	check(t, err)
	effects := &sessionEffects{db: db}
	foreign, err := effects.CreateTeamSession(ctx, sessionKey("predicted"), "foreign-launch")
	check(t, err)
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	recovery := Reconciler{e, sessions, messages}
	check(t, recovery.Reconcile(ctx, 100))
	profile, err := reg.Lookup(ctx, victim.URN)
	check(t, err)
	if profile.Status != registry.StatusActive {
		t.Fatal("foreign registry entry retired")
	}
	binding, err := reg.CurrentBinding(ctx, victim.URN)
	check(t, err)
	if binding.SessionID != "foreign" {
		t.Fatal("foreign binding revoked")
	}
	row, err := db.GetSession(foreign.SessionID)
	check(t, err)
	if row.State != "created" {
		t.Fatal("foreign session killed")
	}
	var n int
	check(t, db.DB().QueryRow(`SELECT count(*) FROM team_port_intents`).Scan(&n))
	if n != 0 {
		t.Fatal("sweep fabricated receipts")
	}
	enrolled, err := e.Ensure(ctx, fresh("predicted"))
	check(t, err)
	if string(enrolled.Actor) == victim.URN {
		t.Fatal("predictable external key adopted")
	}
	check(t, e.AcquireBinding(ctx, "predicted", enrolled.Actor))
	id, err := sessions.Launch(ctx, teamhost.SessionRequest{IntentKey: "predicted", Actor: enrolled.Actor, Provision: fresh("predicted").Provision})
	check(t, err)
	if id == foreign.SessionID {
		t.Fatal("predictable session key adopted")
	}
	check(t, sessions.Stop(ctx, "predicted"))
	check(t, e.Retire(ctx, "predicted"))
	profile, err = reg.Lookup(ctx, victim.URN)
	check(t, err)
	if profile.Status != registry.StatusActive {
		t.Fatal("retire removed foreign URN")
	}
	binding, err = reg.CurrentBinding(ctx, victim.URN)
	check(t, err)
	if binding.SessionID != "foreign" {
		t.Fatal("cleanup touched foreign binding")
	}
}

func TestFreshProvenanceAndPinAdmission(t *testing.T) {
	for _, tc := range []string{"nonce", "pin", "substrate", "retire"} {
		t.Run(tc, func(t *testing.T) {
			db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
			e := enroller(t, db, s, reg)
			req := fresh("private")
			receipt, err := e.reserve(ctx, "enrollment", req.IntentKey, req)
			check(t, err)
			raw, _ := json.Marshal(pin)
			props := map[string]string{"team_definition": string(raw), "team_provenance": receipt.nonce}
			if tc == "nonce" {
				props["team_provenance"] = "foreign"
			}
			if tc == "pin" {
				props["team_definition"] = `{"id":"other"}`
			}
			profile, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Claimed", Props: props}, enrollmentSubstrate, receipt.nonce)
			check(t, err)
			if tc == "substrate" {
				profile.ExternalIDs = []registry.ExternalID{{Substrate: "foreign", ExternalID: receipt.nonce}}
				if matchesProvenance(profile, receipt.nonce) {
					t.Fatal("foreign substrate admitted")
				}
				return
			}
			if tc == "retire" {
				check(t, e.recordAcquisition(ctx, req.IntentKey, receipt.nonce, profile.URN))
				_, err = db.DB().Exec(`UPDATE registry_entries SET props_json=? WHERE urn=?`, `{"team_provenance":"foreign"}`, profile.URN)
				check(t, err)
				check(t, e.Retire(ctx, req.IntentKey))
				profile, err = reg.Lookup(ctx, profile.URN)
				check(t, err)
				if profile.Status != registry.StatusActive {
					t.Fatal("nonmatching provenance retired")
				}
				return
			}
			if _, err = e.Ensure(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
				t.Fatal("untrusted acquisition admitted", err)
			}
		})
	}
}
func TestStableAdmissionAndRetainedActorChecks(t *testing.T) {
	for _, tc := range []string{"inactive", "kind", "actor", "pin"} {
		t.Run(tc, func(t *testing.T) {
			db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
			e := enroller(t, db, s, reg)
			raw, _ := json.Marshal(pin)
			kind := registry.KindAgent
			if tc == "kind" {
				kind = registry.KindProject
			}
			p, _, err := reg.RegisterIdempotent(ctx, kind, registry.Profile{DisplayName: "Existing", Props: map[string]string{"team_definition": string(raw)}}, "caller", "existing")
			check(t, err)
			if tc == "inactive" {
				_, err = reg.Deregister(ctx, p.URN)
				check(t, err)
			}
			req := fresh(tc)
			req.Provision.Slot.Resolution = teams.Durable
			req.Actor = mesh.URN(p.URN)
			req.Provision.Identity = req.Actor
			if tc == "actor" {
				req.Provision.Identity = "msg://agent/local/another"
			}
			if tc == "pin" {
				req.Provision.Slot.Definition.Digest = "different"
			}
			if _, err = e.Ensure(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
				t.Fatal("invalid stable enrollment admitted", err)
			}
		})
	}
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	value, err := e.Ensure(ctx, fresh("owned"))
	check(t, err)
	if err = e.AcquireBinding(ctx, "owned", "msg://agent/local/other"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("binding actor substituted", err)
	}
	if _, err = e.LaunchTarget(ctx, "owned", "msg://agent/local/other"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("launch actor substituted", err)
	}
	check(t, e.ReleaseBinding(ctx, "never-enrolled"))
	if _, err = e.Ensure(ctx, fresh("never-enrolled")); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("binding-only tombstone decoded NULL request", err)
	}
	if value.Actor == "" {
		t.Fatal("empty enrollment")
	}
}
func TestCompletionFailurePreservesRetryableSession(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, _, effects, req := sessionFixture(t, db, s, reg, "busy")
	_, err := db.DB().Exec(`CREATE TRIGGER fail_session_ack BEFORE UPDATE OF payload ON team_port_intents WHEN NEW.port_kind='session' BEGIN SELECT RAISE(ABORT,'busy'); END`)
	check(t, err)
	if _, err = port.Launch(ctx, req); err == nil {
		t.Fatal("completion failure ignored")
	}
	record, err := db.GetSessionIdempotency(testSessionKey(t, port, req.IntentKey))
	check(t, err)
	row, err := db.GetSession(record.SessionID)
	check(t, err)
	if row.State != "created" {
		t.Fatal("completion failure killed acquisition", row.State)
	}
	receipt, err := port.read(ctx, "session", req.IntentKey)
	check(t, err)
	if receipt.state != "pending" {
		t.Fatal("retryable receipt cleaned")
	}
	_, err = db.DB().Exec(`DROP TRIGGER fail_session_ack`)
	check(t, err)
	id, err := port.Launch(ctx, req)
	check(t, err)
	if id != row.ID || effects.launched != 1 {
		t.Fatal("retry lost acquisition")
	}
}

func TestDeliveryRecoveryNeverBypassesHostAndPermanentRefusalsStop(t *testing.T) {
	for _, outcome := range []string{"dead", "dispatched", "failed-delegate"} {
		t.Run(outcome, func(t *testing.T) {
			db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
			e := enroller(t, db, s, reg)
			sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
			check(t, err)
			q := &queueEffects{db: db}
			messages, err := NewMessenger(db.DB(), s, q)
			check(t, err)
			d := delivery(outcome)
			_, err = messages.reserve(ctx, "delivery", d.IdempotencyKey, d)
			check(t, err)
			raw, _ := json.Marshal(d)
			dead, dispatched := 0, 0
			if outcome == "dead" {
				dead = 1
			}
			if outcome == "dispatched" {
				dispatched = 1
			}
			_, err = db.DB().Exec(`INSERT INTO team_host_deliveries(delivery_key,payload,dead,dispatched) VALUES(?,?,?,?)`, d.IdempotencyKey, raw, dead, dispatched)
			check(t, err)
			if outcome == "failed-delegate" {
				_, err = db.DB().Exec(`INSERT INTO team_host_delegations VALUES(?,'failed')`, d.IdempotencyKey)
				check(t, err)
			}
			check(t, (Reconciler{e, sessions, messages}).Reconcile(ctx, 100))
			if q.calls != 0 {
				t.Fatal("terminal host outcome re-delivered")
			}
		})
	}
	db, s, _ := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	q := &refusedMessages{err: teamhost.ErrSessionGone}
	m, err := NewMessenger(db.DB(), s, q)
	check(t, err)
	for range 3 {
		if err = m.Deliver(ctx, delivery("gone")); !errors.Is(err, teamhost.ErrSessionGone) {
			t.Fatal(err)
		}
	}
	if q.calls != 1 {
		t.Fatal("permanent refusal re-driven", q.calls)
	}
	q.err = teamhost.ErrInvalidRequest
	for range 2 {
		if err = m.Deliver(ctx, delivery("invalid")); !errors.Is(err, teamhost.ErrInvalidRequest) {
			t.Fatal("terminal refusal lost error class", err)
		}
	}
	if q.calls != 2 {
		t.Fatal("invalid request re-driven")
	}
}

type refusedMessages struct {
	calls int
	err   error
}

func (q *refusedMessages) QueueTeamDelivery(context.Context, teams.Delivery) error {
	q.calls++
	return q.err
}
func TestSessionKeyConflictIsPermanent(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, _, _, req := sessionFixture(t, db, s, reg, "collision")
	receipt, err := port.reserve(ctx, "session", req.IntentKey, req)
	check(t, err)
	check(t, db.CreateSessionKeyed(store.SessionRow{ID: "foreign", State: "created"}, nil, &store.SessionIdempotency{Key: sessionKey(receipt.nonce), Operation: store.IdempotencyOpCreate, RequestDigest: "other"}))
	port.service = conflictingSessions{}
	if _, err = port.Launch(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) || !errors.Is(err, teamhost.ErrPermanent) || !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatal("collision remains retryable", err)
	}
}

type conflictingSessions struct{ SessionService }

func (conflictingSessions) CreateTeamSession(context.Context, string, string) (*app.Launched, error) {
	return nil, store.ErrIdempotencyConflict
}

func TestRecoveryLeavesAgentTransportUnderHostAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, s, _ := dbFixture(t, path)
	q := &queueEffects{db: db, fault: errors.New("lost acknowledgement")}
	messages, err := NewMessenger(db.DB(), s, q)
	check(t, err)
	d := delivery("agent-recovery")
	if err = messages.Deliver(ctx, d); err == nil {
		t.Fatal("lost acknowledgement ignored")
	}
	check(t, db.Close())
	db, s, reg := dbFixture(t, path)
	q = &queueEffects{db: db}
	messages, err = NewMessenger(db.DB(), s, q)
	check(t, err)
	e := enroller(t, db, s, reg)
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	check(t, (Reconciler{e, sessions, messages}).Reconcile(ctx, 100))
	saved, err := messages.read(ctx, "delivery", d.IdempotencyKey)
	check(t, err)
	if q.calls != 0 || saved.state != "pending" {
		t.Fatal("recovery dispatched or adopted agent delivery")
	}
	// Only an explicit host dispatch retry may complete the lost port receipt.
	check(t, messages.Deliver(ctx, d))
	var count int
	check(t, db.DB().QueryRow(`SELECT count(*) FROM routing_replies WHERE idempotency_key=? AND target_session_id=?`, d.IdempotencyKey, d.Recipient.SessionID).Scan(&count))
	if count != 1 || q.calls != 1 {
		t.Fatal("host retry duplicated or retargeted delivery", count, q.calls)
	}
	_, err = db.DB().Exec(`DELETE FROM team_port_intents WHERE port_kind='delivery' AND intent_key=?`, d.IdempotencyKey)
	check(t, err)
	check(t, (Reconciler{e, sessions, messages}).Reconcile(ctx, 100))
	if _, err = messages.read(ctx, "delivery", d.IdempotencyKey); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("agent transport fabricated a receipt", err)
	}
}

func TestReceiptSequenceNeverReusesDeletedHighWaterMark(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	first, err := e.reserve(ctx, "enrollment", "first", fresh("first"))
	check(t, err)
	if len(first.nonce) != 64 {
		t.Fatal("receipt lacks a random 256-bit nonce")
	}
	retry, err := e.reserve(ctx, "enrollment", "first", fresh("first"))
	check(t, err)
	if retry.nonce != first.nonce {
		t.Fatal("retry changed committed nonce")
	}
	var previous, next int64
	check(t, db.DB().QueryRow(`SELECT sequence FROM team_port_intents WHERE intent_key='first'`).Scan(&previous))
	_, err = db.DB().Exec(`DELETE FROM team_port_intents WHERE intent_key='first'`)
	check(t, err)
	second, err := e.reserve(ctx, "enrollment", "second", fresh("second"))
	check(t, err)
	check(t, db.DB().QueryRow(`SELECT sequence FROM team_port_intents WHERE intent_key='second'`).Scan(&next))
	if next <= previous || second.nonce == first.nonce {
		t.Fatal("new receipt reused cursor position or nonce", previous, next)
	}
}

func TestTransportContentRefusalsArePermanent(t *testing.T) {
	for _, cause := range []error{teams.ErrConflict, messageDelivery.ErrDigestConflict, messageDelivery.ErrInvalidArgument} {
		t.Run(cause.Error(), func(t *testing.T) {
			db, s, _ := dbFixture(t, filepath.Join(t.TempDir(), "db"))
			q := &refusedMessages{err: cause}
			m, err := NewMessenger(db.DB(), s, q)
			check(t, err)
			if err = m.Deliver(ctx, delivery("content-conflict")); !errors.Is(err, teamhost.ErrPermanent) || !errors.Is(err, cause) {
				t.Fatal("permanent transport error lost classification", err)
			}
			if err = m.Deliver(ctx, delivery("content-conflict")); !errors.Is(err, teamhost.ErrPermanent) || q.calls != 1 {
				t.Fatal("permanent transport refusal re-driven", err, q.calls)
			}
		})
	}
}

type permanentSessionFailure struct {
	SessionService
	stage string
	cause error
}

func (s permanentSessionFailure) LinkTeamSession(ctx context.Context, id, key, parent string) error {
	if s.stage == "link" {
		return s.cause
	}
	return s.SessionService.LinkTeamSession(ctx, id, key, parent)
}
func (s permanentSessionFailure) LaunchSessionWithContext(context.Context, string) (*app.Launched, error) {
	return nil, s.cause
}

func TestSessionLifecycleRefusalsArePermanent(t *testing.T) {
	for _, stage := range []string{"link", "launch"} {
		for _, cause := range []error{store.ErrSessionNotFound, session.ErrNotCreated} {
			t.Run(stage+"/"+cause.Error(), func(t *testing.T) {
				db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
				port, _, effects, req := sessionFixture(t, db, s, reg, "missing")
				port.service = permanentSessionFailure{effects, stage, cause}
				if _, err := port.Launch(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) || !errors.Is(err, teamhost.ErrPermanent) || !errors.Is(err, cause) {
					t.Fatal("permanent lifecycle refusal lost classification", err)
				}
			})
		}
	}
}

func TestAcquisitionReceiptRejectsChangedNonceAndActor(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	r, err := e.reserve(ctx, "enrollment", "acquisition", fresh("acquisition"))
	check(t, err)
	if err = e.recordAcquisition(ctx, "acquisition", "foreign", "msg://agent/local/foreign"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("foreign nonce recorded ownership", err)
	}
	check(t, e.recordAcquisition(ctx, "acquisition", r.nonce, "msg://agent/local/owned"))
	check(t, e.recordAcquisition(ctx, "acquisition", r.nonce, "msg://agent/local/owned"))
	if err = e.recordAcquisition(ctx, "acquisition", r.nonce, "msg://agent/local/other"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("recorded actor changed", err)
	}
}

func TestEnsureCompletionRefusesConcurrentBindingFence(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	req := fresh("binding-ended-during-ensure")
	e.definitions = &verifiedDefinitions{before: func() { check(t, e.end(ctx, "enrollment", req.IntentKey, "", true)) }}
	if _, err := e.Ensure(ctx, req); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("completion ignored binding fence", err)
	}
	r, err := e.read(ctx, "enrollment", req.IntentKey)
	check(t, err)
	if len(r.payload) != 0 {
		t.Fatal("fenced acquisition completed")
	}
	check(t, e.Retire(ctx, req.IntentKey))
}

func TestStopWithoutAcquisitionCannotAdoptEmptyNonceSession(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	effects := &sessionEffects{db: db}
	foreign, err := effects.CreateTeamSession(ctx, sessionKey(""), "foreign-launch")
	check(t, err)
	port, err := NewSessions(db, s, effects, e)
	check(t, err)
	check(t, port.Stop(ctx, "never-acquired"))
	row, err := db.GetSession(foreign.SessionID)
	check(t, err)
	if row.State != "created" {
		t.Fatal("nonce-less tombstone killed foreign session")
	}
}
