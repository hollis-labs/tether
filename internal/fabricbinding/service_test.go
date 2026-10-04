package fabricbinding

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/agentdef"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/store"
)

const owner mesh.URN = "msg://user/example/owner"
const agentURN mesh.URN = "msg://agent/example/stable"

var pin = mesh.DefinitionRef{ID: "fixture", Revision: "r1", Digest: "sha256:" + strings.Repeat("a", 64)}
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

type definitions struct {
	hook func()
	err  error
}

func (d *definitions) Load(ctx context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	if ctx.Err() != nil {
		return definitionresolve.VerifiedDefinition{}, ctx.Err()
	}
	if d.hook != nil {
		d.hook()
	}
	if d.err != nil {
		return definitionresolve.VerifiedDefinition{}, d.err
	}
	return definitionresolve.VerifiedDefinition{Pin: p, Definition: &agentdef.Definition{}}, nil
}

type fixture struct {
	db *sql.DB

	s1, s2 *Service
	r1, r2 *fabricstore.Repository
	c      *clock
	defs   *definitions
}

func setup(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	f := &fixture{db: first.DB(), r1: fabricstore.New(first.DB()), r2: fabricstore.New(second.DB()), c: &clock{at: epoch}, defs: &definitions{}}
	err = f.r1.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if err := tx.AddDefinition(fabricstore.DefinitionRevision{Definition: pin, SourceRef: "urn:source:fixture"}); err != nil {
			return err
		}
		if err := tx.PutActor(fabricstore.Actor{Actor: mesh.Actor{URN: agentURN, Kind: mesh.ActorAgent}, Owner: owner, Lifecycle: mesh.EnrollmentActive}, 0); err != nil {
			return err
		}
		return tx.PutAgent(mesh.Agent{URN: agentURN, Owner: owner, Definition: pin, Lifecycle: mesh.EnrollmentActive}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := func(_ context.Context, a Authorization) error {
		if a.Caller != a.Owner {
			return errors.New("private policy refused")
		}
		return nil
	}
	validate := func(context.Context, mesh.URN, Observation) error { return nil }
	f.s1, err = New(f.r1, f.defs, auth, validate, f.c.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.s2, err = New(f.r2, f.defs, auth, validate, f.c.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func request(key string) AdmissionRequest {
	return AdmissionRequest{AgentURN: agentURN, Key: key, NodeRef: "urn:node:fixture", RuntimeRef: "urn:runtime:fixture", LaunchRecordRef: "urn:launch:" + key, ContextRef: "urn:context:" + key, StoreRef: "urn:store:fixture", TTL: time.Minute}
}
func admitted(t *testing.T, f *fixture, key string) Reservation {
	t.Helper()
	r, err := f.s1.Admit(t.Context(), owner, request(key))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func authority(t *testing.T, f *fixture, r Reservation) Authority {
	t.Helper()
	return Authority{AgentURN: r.Lease.AgentURN, SessionURN: r.Lease.SessionURN, InstanceID: r.Lease.InstanceID, Fence: r.Lease.FencingToken}
}
func observeErr(ctx context.Context, port HostPort, caller mesh.URN, o Observation) error {
	_, err := port.Observe(ctx, caller, o)
	return err
}
func observation(t *testing.T, f *fixture, r Reservation, status mesh.InstanceStatus, detail mesh.InstanceDetail, state mesh.SessionState) Observation {
	t.Helper()
	session, err := f.r1.Session(t.Context(), r.Session.URN)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := f.r1.Instance(t.Context(), r.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	return Observation{Authority: authority(t, f, r), Status: status, Detail: detail, SessionState: state, SessionVersion: session.Version, InstanceVersion: instance.Version, ReportRef: "urn:report:fixture"}
}
func TestExclusiveAcquireAcrossIndependentConnections(t *testing.T) {
	f := setup(t)
	start := make(chan struct{})
	type answer struct {
		r   Reservation
		err error
	}
	out := make(chan answer, 2)
	for i, s := range []*Service{f.s1, f.s2} {
		go func(s *Service, key string) {
			<-start
			r, err := s.Admit(t.Context(), owner, request(key))
			out <- answer{r, err}
		}(s, []string{"left", "right"}[i])
	}
	close(start)
	one, two := <-out, <-out
	if one.err != nil {
		one, two = two, one
	}
	if one.err != nil || !errors.Is(two.err, ErrBound) {
		t.Fatal(one.err, two.err)
	}
	head, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.Lease == nil || head.Value.Lease.InstanceID != one.r.Instance.ID || head.Value.HighWater != one.r.Lease.FencingToken {
		t.Fatal(head, err)
	}
	loser := "left"
	if one.r.Instance.ID == executionID(owner, "left") {
		loser = "right"
	}
	if _, err := f.r2.Instance(t.Context(), executionID(owner, loser)); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("losing instance persisted", err)
	}
	if _, err := f.r2.Session(t.Context(), newSessionURN(owner, loser)); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("losing session persisted", err)
	}
}
func TestConcurrentIdempotencyCallerScopeAndRetryDoesNotReacquire(t *testing.T) {
	f := setup(t)
	start := make(chan struct{})
	results := make(chan Reservation, 2)
	errs := make(chan error, 2)
	for _, s := range []*Service{f.s1, f.s2} {
		go func(s *Service) {
			<-start
			r, err := s.Admit(t.Context(), owner, request("same"))
			results <- r
			errs <- err
		}(s)
	}
	close(start)
	first, second := <-results, <-results
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("retry changed reservation", first, second)
	}
	changed := request("same")
	changed.NodeRef = "urn:node:changed"
	if _, err := f.s1.Admit(t.Context(), owner, changed); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("key reused for changed request", err)
	}
	f.c.Advance(2 * time.Minute)
	f.defs.err = errors.New("source unavailable")
	retry, err := f.s2.Admit(t.Context(), owner, request("same"))
	if err != nil || retry.Lease != first.Lease || retry.Instance.ID != first.Instance.ID || retry.Current {
		t.Fatal(retry, err)
	}
	head, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.HighWater != first.Lease.FencingToken || head.Value.Lease.ExpiresAt != first.Lease.ExpiresAt {
		t.Fatal("retry extended expired lease", head, err)
	}
	f.defs.err = nil
	other := mesh.URN("msg://user/example/delegated")
	f.s2.authorize = func(context.Context, Authorization) error { return nil }
	r := request("same")
	next, err := f.s2.Admit(t.Context(), other, r)
	if err != nil || next.OperationRef == first.OperationRef || next.Lease.Holder != other || next.Lease.FencingToken <= first.Lease.FencingToken {
		t.Fatal(next, err)
	}
	old, err := f.r1.Instance(t.Context(), first.Instance.ID)
	if err != nil || old.Value.Status != mesh.InstanceStarting {
		t.Fatal("expiry fabricated process death", old, err)
	}
}
func TestEveryWriterRejectsStaleReusedWrongHolderAndExpiredAuthority(t *testing.T) {
	for _, bad := range []string{"fence", "session", "instance", "holder", "expired"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			r := admitted(t, f, "first")
			a := authority(t, f, r)
			caller := owner
			switch bad {
			case "fence":
				a.Fence++
			case "session":
				a.SessionURN = "urn:session:other"
			case "instance":
				a.InstanceID = "other"
			case "holder":
				caller = "msg://user/example/delegated"
				f.s1.authorize = func(context.Context, Authorization) error { return nil }
			case "expired":
				f.c.Advance(time.Minute)
			}
			o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
			o.Authority = a
			ops := []func() error{func() error { _, e := f.s1.RenewLease(t.Context(), caller, a, 3*time.Minute); return e }, func() error { return f.s1.ReleaseLease(t.Context(), caller, a, "urn:proof:quiesced") }, func() error { return observeErr(t.Context(), f.s1, caller, o) }, func() error { return f.s1.ReportReference(t.Context(), caller, a, "urn:report:fixture") }}
			for _, op := range ops {
				if err := op(); !errors.Is(err, ErrStale) {
					t.Fatal("stale writer accepted", bad, err)
				}
			}
			instance, err := f.r1.Instance(t.Context(), r.Instance.ID)
			if err != nil || instance.Value.Status != mesh.InstanceStarting {
				t.Fatal(instance, err)
			}
		})
	}
	f := setup(t)
	old := admitted(t, f, "old")
	a := authority(t, f, old)
	late := observation(t, f, old, mesh.InstanceStopped, mesh.InstanceDetail{Stopped: mesh.StopCompleted}, mesh.SessionEnded)
	f.c.Advance(2 * time.Minute)
	current := admitted(t, f, "new")
	if current.Lease.FencingToken <= old.Lease.FencingToken {
		t.Fatal("fence reused")
	}
	if err := observeErr(t.Context(), f.s1, owner, late); !errors.Is(err, ErrStale) {
		t.Fatal("late terminal changed new binding", err)
	}
	if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:late"); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	head, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.Lease.InstanceID != current.Instance.ID {
		t.Fatal(head, err)
	}
}
func TestRenewReleasedFencePersistenceResumePinsAndTerminalRelease(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	a := authority(t, f, r)
	f.c.Advance(10 * time.Second)
	lease, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute)
	if err != nil || lease.FencingToken != a.Fence || !lease.ExpiresAt.Equal(epoch.Add(130*time.Second)) {
		t.Fatal(lease, err)
	}
	if _, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("identical renewal not refused", err)
	}
	if _, err := f.s1.RenewLease(t.Context(), owner, a, 3*time.Minute); err != nil {
		t.Fatal("same holder renew conflicts", err)
	}
	o := observation(t, f, r, mesh.InstanceWaiting, mesh.InstanceDetail{Waiting: mesh.WaitingApproval}, mesh.SessionPaused)
	if err := observeErr(t.Context(), f.s1, owner, o); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s2.Admit(t.Context(), owner, request("other")); !errors.Is(err, ErrBound) {
		t.Fatal("paused waiting holder lost lease", err)
	}
	o = observation(t, f, r, mesh.InstanceStopped, mesh.InstanceDetail{Stopped: mesh.StopCanceled}, mesh.SessionEnded)
	if err := observeErr(t.Context(), f.s1, owner, o); err != nil {
		t.Fatal(err)
	}
	head, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.Lease != nil || head.Value.HighWater != a.Fence {
		t.Fatal("terminal did not conditionally release", head, err)
	}
	freshService, err := New(f.r2, f.defs, f.s2.authorize, f.s2.validate, f.c.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := request("resume")
	req.SessionURN = r.Session.URN
	req.ContextRef = r.Session.ContextRef
	req.StoreRef = r.Session.StoreRef
	resumed, err := freshService.Admit(t.Context(), owner, req)
	if err != nil || resumed.Session.URN != r.Session.URN || resumed.Session.Definition != r.Session.Definition || resumed.Instance.ID == r.Instance.ID || resumed.Lease.FencingToken <= a.Fence {
		t.Fatal(resumed, err)
	}
	old, err := f.r1.Instance(t.Context(), r.Instance.ID)
	if err != nil || old.Value.Status != mesh.InstanceStopped || old.Value.Detail.Stopped != mesh.StopCanceled {
		t.Fatal("terminal history reopened", old, err)
	}
	if err := freshService.ReleaseLease(t.Context(), owner, authority(t, f, resumed), "urn:proof:quiesced"); err != nil {
		t.Fatal(err)
	}
	head, err = f.r1.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.Lease != nil || head.Value.HighWater != resumed.Lease.FencingToken {
		t.Fatal(head, err)
	}
}
func TestRecordingAuthorizerAndHostPortPolicies(t *testing.T) {
	f := setup(t)
	var calls []Authorization
	f.s1.authorize = func(_ context.Context, a Authorization) error { calls = append(calls, a); return nil }
	var portCalls []Observation
	var holders []mesh.URN
	f.s1.validate = func(_ context.Context, holder mesh.URN, o Observation) error {
		holders = append(holders, holder)
		portCalls = append(portCalls, o)
		return nil
	}
	req := request("first")
	r, err := f.s1.Admit(t.Context(), owner, req)
	if err != nil {
		t.Fatal(err)
	}
	acquire := admissionAuth(owner, mesh.Agent{Owner: owner}, req, pin)
	if !reflect.DeepEqual(calls[0], acquire) {
		t.Fatal(calls[0], acquire)
	}
	a := authority(t, f, r)
	var host HostPort = f.s1
	o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
	if err := observeErr(t.Context(), host, owner, o); err != nil {
		t.Fatal(err)
	}
	if err := host.ReportReference(t.Context(), owner, a, "urn:report:result"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(portCalls, []Observation{o, {Authority: a, ReportRef: "urn:report:result"}}) || !reflect.DeepEqual(holders, []mesh.URN{owner, owner}) {
		t.Fatal(portCalls, holders)
	}
	for i, action := range []Action{Lifecycle, Report} {
		want := Authorization{Caller: owner, Owner: owner, AgentURN: agentURN, SessionURN: r.Session.URN, InstanceID: r.Instance.ID, Action: action, Fence: a.Fence}
		if !reflect.DeepEqual(calls[i+1], want) {
			t.Fatal(calls[i+1], want)
		}
	}
	f.c.Advance(time.Second)
	if _, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	a = authority(t, f, r)
	if err := f.s1.ReleaseLease(t.Context(), owner, a, "urn:proof:quiesced"); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls[3:] {
		if call.Caller != owner || call.Owner != owner || call.AgentURN != agentURN || call.SessionURN != r.Session.URN || call.InstanceID != r.Instance.ID || call.Fence != a.Fence {
			t.Fatal(call)
		}
	}
	if calls[3].Action != Renew || calls[4].Action != Release || calls[4].ProofRef != "urn:proof:quiesced" {
		t.Fatal(calls)
	}
}
func TestDenialsAreOpaqueAndOperationalFaultsPropagate(t *testing.T) {
	f := setup(t)
	stranger := mesh.URN("msg://user/example/stranger")
	for _, urn := range []mesh.URN{agentURN, "msg://agent/example/missing"} {
		req := request("denied")
		req.AgentURN = urn
		_, err := f.s1.Admit(t.Context(), stranger, req)
		if !errors.Is(err, ErrDenied) || err.Error() != ErrDenied.Error() {
			t.Fatal("existence oracle", err)
		}
	}
	r := admitted(t, f, "first")
	o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
	for _, fault := range []error{errors.New("private sink unavailable"), context.Canceled, context.DeadlineExceeded} {
		f.s1.validate = func(context.Context, mesh.URN, Observation) error { return fault }
		err := observeErr(t.Context(), f.s1, owner, o)
		if errors.Is(fault, context.Canceled) || errors.Is(fault, context.DeadlineExceeded) {
			if !errors.Is(err, fault) {
				t.Fatal(err)
			}
		} else {
			if !errors.Is(err, ErrDenied) || strings.Contains(err.Error(), "private sink") {
				t.Fatal(err)
			}
		}
	}
	instance, err := f.r1.Instance(t.Context(), r.Instance.ID)
	if err != nil || instance.Value.Status != mesh.InstanceStarting {
		t.Fatal("sink denial persisted state", instance, err)
	}
	f.defs.err = errors.New("read failure")
	f.c.Advance(2 * time.Minute)
	if _, err := f.s1.Admit(t.Context(), owner, request("io")); !errors.Is(err, f.defs.err) {
		t.Fatal("real I/O swallowed", err)
	}
}
func TestClockBeforeWorkVersionsRollbackAndTextValidation(t *testing.T) {
	f := setup(t)
	f.defs.hook = func() { f.c.Advance(30 * time.Second) }
	r := admitted(t, f, "timing")
	if !r.Lease.ExpiresAt.Equal(epoch.Add(time.Minute)) || !r.Instance.CreatedAt.Equal(epoch) {
		t.Fatal("freshness stamped after I/O", r)
	}
	f.defs.hook = nil
	o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
	wrong := o
	wrong.InstanceVersion++
	if err := observeErr(t.Context(), f.s1, owner, wrong); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("instance CAS ignored", err)
	}
	wrong = o
	wrong.SessionVersion++
	if err := observeErr(t.Context(), f.s1, owner, wrong); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("session CAS ignored", err)
	}
	f.s1.validate = func(context.Context, mesh.URN, Observation) error { f.c.Advance(time.Minute); return nil }
	if err := observeErr(t.Context(), f.s1, owner, o); !errors.Is(err, ErrStale) {
		t.Fatal("policy consumed expiry and write was accepted", err)
	}
	for _, value := range []string{"invalid\xff", strings.Repeat("x", maxTextBytes+1)} {
		for _, field := range []string{"key", "node", "runtime", "launch", "context", "store", "session", "agent"} {
			req := request("bad")
			switch field {
			case "key":
				req.Key = value
			case "node":
				req.NodeRef = value
			case "runtime":
				req.RuntimeRef = value
			case "launch":
				req.LaunchRecordRef = value
			case "context":
				req.ContextRef = value
			case "store":
				req.StoreRef = value
			case "session":
				req.SessionURN = mesh.URN(value)
			case "agent":
				req.AgentURN = mesh.URN(value)
			}
			if _, err := f.s1.Admit(t.Context(), owner, req); !errors.Is(err, fabricstore.ErrInvalid) {
				t.Fatal(field, err)
			}
		}
		if err := f.s1.ReleaseLease(t.Context(), owner, o.Authority, value); !errors.Is(err, fabricstore.ErrInvalid) {
			t.Fatal(err)
		}
		if err := f.s1.ReportReference(t.Context(), owner, o.Authority, value); !errors.Is(err, fabricstore.ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := f.s1.Admit(t.Context(), mesh.URN(value), request("caller")); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
}

func TestOutboxFailureRollsBackEveryWriterAndReservation(t *testing.T) {
	for _, action := range []Action{Acquire, Renew, Release, Lifecycle, Report} {
		t.Run(string(action), func(t *testing.T) {
			f := setup(t)
			var r Reservation
			if action != Acquire {
				r = admitted(t, f, "first")
			}
			// A storage fault after state writes must roll the entire operation back.
			var err error
			if _, err = f.db.ExecContext(t.Context(), `CREATE TRIGGER refuse_outbox BEFORE INSERT ON fabric_outbox BEGIN SELECT RAISE(ABORT,'fixture outbox unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			var before fabricstore.Snapshot
			if action != Acquire {
				before, err = f.r1.Snapshot(t.Context(), agentURN, r.Session.URN)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch action {
			case Acquire:
				_, err = f.s1.Admit(t.Context(), owner, request("first"))
			case Renew:
				f.c.Advance(time.Second)
				_, err = f.s1.RenewLease(t.Context(), owner, authority(t, f, r), 2*time.Minute)
			case Release:
				err = f.s1.ReleaseLease(t.Context(), owner, authority(t, f, r), "urn:proof:quiesced")
			case Lifecycle:
				err = observeErr(t.Context(), f.s1, owner, observation(t, f, r, mesh.InstanceStopped, mesh.InstanceDetail{Stopped: mesh.StopCompleted}, mesh.SessionEnded))
			case Report:
				err = f.s1.ReportReference(t.Context(), owner, authority(t, f, r), "urn:report:fixture")
			}
			if err == nil || errors.Is(err, ErrDenied) {
				t.Fatal("outbox fault swallowed", err)
			}
			if action == Acquire {
				if _, err = f.r2.Instance(t.Context(), executionID(owner, "first")); !errors.Is(err, fabricstore.ErrNotFound) {
					t.Fatal(err)
				}
				if _, err = f.r2.Session(t.Context(), newSessionURN(owner, "first")); !errors.Is(err, fabricstore.ErrNotFound) {
					t.Fatal(err)
				}
				if _, err = f.r2.Admission(t.Context(), owner, "first"); !errors.Is(err, fabricstore.ErrNotFound) {
					t.Fatal(err)
				}
				if _, err = f.r2.BindingHead(t.Context(), agentURN); !errors.Is(err, fabricstore.ErrNotFound) {
					t.Fatal(err)
				}
			} else {
				after, e := f.r2.Snapshot(t.Context(), agentURN, r.Session.URN)
				if e != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("outbox failure committed mutation", before, after, e)
				}
			}
		})
	}
}

func TestFenceExhaustionCannotWrapOrReserve(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	head, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil {
		t.Fatal(err)
	}
	const exhausted = uint64(1<<63 - 1)
	instance := r.Instance
	instance.ID = "exhaustion-fixture"
	instance.BindingFence = exhausted
	lease := r.Lease
	lease.InstanceID = instance.ID
	lease.FencingToken = exhausted
	err = f.r1.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if e := tx.PutInstance(instance, 0); e != nil {
			return e
		}
		return tx.PutBindingHead(fabricstore.BindingHead{AgentURN: agentURN, HighWater: exhausted, Lease: &lease}, head.Version)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.c.Advance(2 * time.Minute)
	before, err := f.r1.BindingHead(t.Context(), agentURN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s2.Admit(t.Context(), owner, request("overflow")); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("exhausted fence wrapped", err)
	}
	after, err := f.r2.BindingHead(t.Context(), agentURN)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(before, after, err)
	}
	if _, err := f.r2.Instance(t.Context(), executionID(owner, "overflow")); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("overflow reserved instance", err)
	}
}
