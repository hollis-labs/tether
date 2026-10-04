package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestPublicEnrollmentNonceCannotAdoptOrRewriteBinding(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	req := fresh("forged-binding")
	enrolled, err := e.Ensure(ctx, req)
	check(t, err)
	r, err := e.read(ctx, "enrollment", req.IntentKey)
	check(t, err)
	profile, err := reg.Lookup(ctx, string(enrolled.Actor))
	check(t, err)
	publicNonce := profile.Props["team_provenance"]
	raw, err := json.Marshal(profile)
	check(t, err)
	if len(r.bindingSecret) != 64 || r.bindingSecret == publicNonce || strings.Contains(string(raw), r.bindingSecret) {
		t.Fatal("binding secret exposed or derived from public nonce")
	}
	foreign, err := reg.LeaseBinding(ctx, string(enrolled.Actor), "foreign-session", "team", publicNonce, nil, registry.VisibilityPublishedLocal, 0)
	check(t, err)
	if err = e.AcquireBinding(ctx, req.IntentKey, enrolled.Actor); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("forged binding adopted", err)
	}
	_, err = e.reserve(ctx, "session", req.IntentKey, teamhost.SessionRequest{IntentKey: req.IntentKey, Actor: enrolled.Actor})
	check(t, err)
	if err = e.BindSession(ctx, req.IntentKey, enrolled.Actor, "rewritten-session"); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("forged binding rewritten", err)
	}
	check(t, e.ReleaseBinding(ctx, req.IntentKey))
	retained, err := reg.CurrentBinding(ctx, string(enrolled.Actor))
	check(t, err)
	if retained.ID != foreign.ID || retained.SessionID != "foreign-session" {
		t.Fatal("forged binding changed", retained)
	}
}

func TestBindingAdoptionAndMutationRequireOwnedAttemptActorAndVisibility(t *testing.T) {
	for _, operation := range []string{"bind", "release", "adopt"} {
		for _, field := range []string{"attempt", "actor", "visibility"} {
			if operation == "adopt" && field == "actor" {
				continue
			}
			t.Run(operation+"/"+field, func(t *testing.T) {
				db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
				_, e, _, req := sessionFixture(t, db, s, reg, "owner")
				_, err := e.reserve(ctx, "session", req.IntentKey, req)
				check(t, err)
				binding, err := reg.CurrentBinding(ctx, string(req.Actor))
				check(t, err)
				r, err := e.read(ctx, "enrollment", req.IntentKey)
				check(t, err)
				var query string
				var value string
				switch field {
				case "attempt":
					query = `UPDATE runtime_bindings SET attempt_id=? WHERE id=?`
					value = r.nonce
				case "actor":
					query = `UPDATE runtime_bindings SET target_urn=? WHERE id=?`
					value = "msg://agent/local/foreign"
				case "visibility":
					query = `UPDATE runtime_bindings SET visibility=? WHERE id=?`
					value = string(registry.VisibilityPublishedLocal)
				}
				_, err = db.DB().Exec(query, value, binding.ID)
				check(t, err)
				switch operation {
				case "bind":
					if err = e.BindSession(ctx, req.IntentKey, req.Actor, "replacement"); !errors.Is(err, teams.ErrUnavailable) {
						t.Fatal("foreign binding rewritten", err)
					}
				case "release":
					check(t, e.ReleaseBinding(ctx, req.IntentKey))
				case "adopt":
					if err = e.AcquireBinding(ctx, req.IntentKey, req.Actor); !errors.Is(err, teams.ErrUnavailable) {
						t.Fatal("foreign binding adopted", err)
					}
				}
				var retainedSession string
				var revoked sql.NullString
				check(t, db.DB().QueryRow(`SELECT session_id,revoked_at FROM runtime_bindings WHERE id=?`, binding.ID).Scan(&retainedSession, &revoked))
				if retainedSession != binding.SessionID || revoked.Valid {
					t.Fatal("ownership fence changed foreign row", retainedSession, revoked)
				}
			})
		}
	}
}

func TestPortErrorClassificationPreservesTransientAndPinsPermanentSentinels(t *testing.T) {
	transient := []error{teams.ErrUnavailable, context.Canceled, context.DeadlineExceeded, errors.New("database is locked (SQLITE_BUSY)")}
	classifiers := []struct {
		name      string
		classify  func(error) error
		permanent []error
	}{
		{"session", classifySession, []error{store.ErrIdempotencyConflict, store.ErrSessionNotFound, session.ErrNotCreated}},
		{"enrollment", classifyEnrollment, []error{registry.ErrNotFound, registry.ErrInvalidRequest, definitionresolve.ErrPinMismatch, definitionresolve.ErrContent, fabricstore.ErrNotFound}},
	}
	for _, tc := range classifiers {
		for _, cause := range transient {
			t.Run(tc.name+"/transient/"+cause.Error(), func(t *testing.T) {
				wrapped := fmt.Errorf("port: %w", cause)
				got := tc.classify(wrapped)
				if !errors.Is(got, wrapped) || !errors.Is(got, cause) || errors.Is(got, teams.ErrProvisionFailed) || errors.Is(got, teamhost.ErrPermanent) {
					t.Fatal("transient refusal made permanent", got)
				}
			})
		}
		for _, cause := range tc.permanent {
			t.Run(tc.name+"/permanent/"+cause.Error(), func(t *testing.T) {
				got := tc.classify(fmt.Errorf("port: %w", cause))
				if !errors.Is(got, cause) || !errors.Is(got, teams.ErrProvisionFailed) {
					t.Fatal("permanent sentinel not classified", got)
				}
				if tc.name == "session" && !errors.Is(got, teamhost.ErrPermanent) {
					t.Fatal("host permanent class lost", got)
				}
			})
		}
	}
}

type rotatingEnroller struct {
	teamhost.Enroller
	calls       []string
	attempts    map[string]int
	transient   map[string]bool
	retireCalls int
}

func (e *rotatingEnroller) Ensure(ctx context.Context, in teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	e.calls = append(e.calls, in.IntentKey)
	e.attempts[in.IntentKey]++
	if e.transient[in.IntentKey] && e.attempts[in.IntentKey] == 1 {
		return teamhost.Enrollment{}, teams.ErrUnavailable
	}
	return e.Enroller.Ensure(ctx, in)
}
func (e *rotatingEnroller) Retire(context.Context, string) error {
	e.retireCalls++
	return teamhost.ErrPermanent
}

func TestReconcileCursorAdvancesPersistsAndWrapsPastTransientFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, s, reg := dbFixture(t, path)
	e := enroller(t, db, s, reg)
	for _, key := range []string{"first", "second", "third", "fourth"} {
		_, err := e.reserve(ctx, "enrollment", key, fresh(key))
		check(t, err)
	}
	// Show the planner's choice for the exact production pending query.
	rows, err := db.DB().Query("EXPLAIN QUERY PLAN "+pendingPortQuery, 0, 2)
	check(t, err)
	for rows.Next() {
		var id, parent, unused int
		var detail string
		check(t, rows.Scan(&id, &parent, &unused, &detail))
		t.Log("EXPLAIN:", detail)
	}
	check(t, errors.Join(rows.Err(), rows.Close()))
	tracked := &rotatingEnroller{Enroller: e, attempts: map[string]int{}, transient: map[string]bool{"first": true, "third": true}}
	makeRecovery := func() Reconciler {
		sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
		check(t, err)
		messages, err := NewMessenger(db.DB(), s, unusedMessages{})
		check(t, err)
		return Reconciler{tracked, sessions, messages}
	}
	r := makeRecovery()
	if err = r.Reconcile(ctx, 2); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("first transient failure lost", err)
	}
	check(t, db.Close())
	db, s, reg = dbFixture(t, path)
	e = enroller(t, db, s, reg)
	tracked.Enroller = e
	r = makeRecovery()
	if err = r.Reconcile(ctx, 2); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("second transient failure lost", err)
	}
	check(t, r.Reconcile(ctx, 2))
	if !reflect.DeepEqual(tracked.calls, []string{"first", "second", "third", "fourth", "first", "third"}) {
		t.Fatal("cursor stalled, skipped or failed to wrap", tracked.calls)
	}
}

func TestEndedPermanentRecoveryBecomesTerminal(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	check(t, e.end(ctx, "enrollment", "ended", "retire", false))
	tracked := &rotatingEnroller{Enroller: e}
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r := Reconciler{tracked, sessions, messages}
	if err = r.Reconcile(ctx, 1); !errors.Is(err, teamhost.ErrPermanent) {
		t.Fatal(err)
	}
	check(t, r.Reconcile(ctx, 1))
	if tracked.retireCalls != 1 {
		t.Fatal("permanent ended receipt retried", tracked.retireCalls)
	}
}

func TestEndedTeamReceiptRefusesStillBoundSessionPrincipal(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, e, _, req := sessionFixture(t, db, s, reg, "ended-principal")
	id, err := port.Launch(ctx, req)
	check(t, err)
	caller := identity.WithPrincipal(ctx, identity.Principal{Kind: "session", SessionID: id, ID: "msg://session/local/" + id})
	resolver := Principals{Mode: identity.Enforce, Sessions: e}
	_, err = resolver.ResolvePrincipal(caller)
	check(t, err)
	// The stop fence commits before the runtime stops or its binding is released.
	check(t, port.end(ctx, "session", req.IntentKey, "stop", false))
	if _, err = resolver.ResolvePrincipal(caller); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("ended receipt authorized session", err)
	}
}

func TestLostAckRetirementAdoptsOnlyMatchingProvenance(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(fmt.Sprint(matching), func(t *testing.T) {
			db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
			e := enroller(t, db, s, reg)
			req := fresh("lost-registration")
			r, err := e.reserve(ctx, "enrollment", req.IntentKey, req)
			check(t, err)
			provenance := r.nonce
			if !matching {
				provenance = "foreign"
			}
			pinJSON, err := json.Marshal(pin)
			check(t, err)
			foreign, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Lost ack", Props: map[string]string{"team_definition": string(pinJSON), "team_provenance": provenance}}, enrollmentSubstrate, r.nonce)
			check(t, err)
			check(t, e.Retire(ctx, req.IntentKey))
			r, err = e.read(ctx, "enrollment", req.IntentKey)
			check(t, err)
			p, err := reg.Lookup(ctx, foreign.URN)
			check(t, err)
			if matching {
				if r.acquired != foreign.URN || p.Status == registry.StatusActive {
					t.Fatal("owned lost acknowledgement not retired")
				}
			} else if r.acquired != "" || p.Status != registry.StatusActive {
				t.Fatal("foreign lost-ack profile adopted or retired")
			}
		})
	}
}
