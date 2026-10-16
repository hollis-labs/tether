package teamruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	mesh "github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

type failingEnroller struct {
	teamhost.Enroller
	cause error
	calls int
}

func (e *failingEnroller) Ensure(c context.Context, r teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	e.calls++
	if e.cause != nil {
		return teamhost.Enrollment{}, e.cause
	}
	return e.Enroller.Ensure(c, r)
}

func TestPortRetriesPersistBackoffDeduplicateAndReset(t *testing.T) {
	path := t.TempDir() + "/db"
	db, s, reg := dbFixture(t, path)
	e := enroller(t, db, s, reg)
	_, err := e.reserve(ctx, "enrollment", "retry", fresh("retry"))
	check(t, err)
	failure := errors.New("unavailable A")
	tracked := &failingEnroller{Enroller: e, cause: failure}
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r := Reconciler{tracked, sessions, messages}
	now := time.Now()
	if err = r.reconcile(ctx, 1, now); !errors.Is(err, failure) {
		t.Fatal("first failure not reported", err)
	}
	check(t, r.reconcile(ctx, 1, now))
	if tracked.calls != 1 {
		t.Fatal("immediate retry")
	}
	check(t, db.Close())
	db, s, reg = dbFixture(t, path)
	e = enroller(t, db, s, reg)
	tracked.Enroller = e
	sessions, err = NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err = NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r = Reconciler{tracked, sessions, messages}
	check(t, r.reconcile(ctx, 1, now))
	if tracked.calls != 1 {
		t.Fatal("restart lost retry delay")
	}
	check(t, r.reconcile(ctx, 1, now.Add(time.Second)))
	if tracked.calls != 2 {
		t.Fatal("due retry missing")
	}
	failure = errors.New("unavailable B")
	tracked.cause = failure
	if err = r.reconcile(ctx, 1, now.Add(3*time.Second)); !errors.Is(err, failure) {
		t.Fatal("changed failure hidden", err)
	}
	tracked.cause = nil
	check(t, r.reconcile(ctx, 1, now.Add(7*time.Second)))
	var count int
	check(t, db.DB().QueryRow(`SELECT count(*) FROM team_recovery_attempts WHERE kind='enrollment' AND intent_key='retry'`).Scan(&count))
	if count != 0 {
		t.Fatal("successful retry retained failure")
	}
}

func TestCleanupBypassesAcquisitionBackoff(t *testing.T) {
	db, s, reg := dbFixture(t, t.TempDir()+"/db")
	e := enroller(t, db, s, reg)
	_, err := e.reserve(ctx, "enrollment", "cleanup", fresh("cleanup"))
	check(t, err)
	tracked := &failingEnroller{Enroller: e, cause: teams.ErrUnavailable}
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	r := Reconciler{tracked, sessions, messages}
	now := time.Now()
	if err = r.reconcile(ctx, 1, now); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal(err)
	}
	check(t, e.end(ctx, "enrollment", "cleanup", "retire", false))
	check(t, r.reconcile(ctx, 1, now))
	receipt, err := e.read(ctx, "enrollment", "cleanup")
	check(t, err)
	if receipt.state != "cleaned" || tracked.calls != 1 {
		t.Fatal("cleanup delayed or acquisition retried", receipt.state, tracked.calls)
	}
}

type stopSpy struct {
	SessionService
	calls int
}

func (s *stopSpy) StopTeamSession(context.Context, string) error {
	s.calls++
	return errors.New("foreign session stop attempted")
}

func TestOrphanDeadlineDoesNotStopUnownedSession(t *testing.T) {
	db, s, reg := dbFixture(t, t.TempDir()+"/db")
	port, e, effects, request := sessionFixture(t, db, s, reg, "orphan")
	spy := &stopSpy{SessionService: effects}
	port.service = spy
	foreign, err := effects.CreateTeamSession(ctx, "unrelated-key", "foreign-launch")
	check(t, err)
	_, err = port.reserve(ctx, "session", request.IntentKey, request)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: port, Messenger: messages, Channels: Channels{}}, teamhost.Options{})
	check(t, err)
	record := teams.LaunchRecord{Key: "orphan-launch", Digest: "digest", State: teams.Planning, Team: teams.Team{ID: "orphan", Version: 1}, Limits: mesh.Limits{MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 100, Timeout: time.Minute}, Deadline: time.Now().Add(-time.Hour), Intents: []teams.MemberIntent{{Key: request.IntentKey, MemberID: request.Provision.MemberID, Slot: request.Provision.Slot, Request: request.Provision}}}
	check(t, s.WithLease(ctx, record.Key, func(c context.Context) error { return s.PutLaunch(c, record) }))
	check(t, host.ReconcileExpiredFormations(ctx, 10))
	check(t, host.ReconcileExpiredFormations(ctx, 10))
	if spy.calls != 0 {
		t.Fatal("unowned session reached StopTeamSession")
	}
	row, err := db.GetSession(foreign.SessionID)
	check(t, err)
	if row.State != "created" {
		t.Fatal("foreign session changed", row.State)
	}
	got, err := s.GetLaunch(ctx, record.Key)
	check(t, err)
	if got.State != teams.Failed || !got.Intents[0].Cleaned {
		t.Fatal("orphan not cleaned", got)
	}
}
