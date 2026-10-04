package fabricbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

func addAgent(t *testing.T, f *fixture, urn, principal mesh.URN) {
	t.Helper()
	err := f.r1.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if e := tx.PutActor(fabricstore.Actor{Actor: mesh.Actor{URN: urn, Kind: mesh.ActorAgent}, Owner: principal, Lifecycle: mesh.EnrollmentActive}, 0); e != nil {
			return e
		}
		return tx.PutAgent(mesh.Agent{URN: urn, Owner: principal, Definition: pin, Lifecycle: mesh.EnrollmentActive}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func retireAgent(t *testing.T, f *fixture) {
	t.Helper()
	err := f.r1.Write(t.Context(), func(tx *fabricstore.Tx) error {
		actor, e := tx.Actor(agentURN)
		if e != nil {
			return e
		}
		agent, e := tx.Agent(agentURN)
		if e != nil {
			return e
		}
		actor.Value.Lifecycle = mesh.EnrollmentRetired
		agent.Value.Lifecycle = mesh.EnrollmentRetired
		if e = tx.PutActor(actor.Value, actor.Version); e != nil {
			return e
		}
		return tx.PutAgent(agent.Value, agent.Version)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestServerDerivedNamesAreScopedAndResumeDoesNotCreate(t *testing.T) {
	f := setup(t)
	first := admitted(t, f, "shared")
	other := mesh.URN("msg://user/example/other")
	otherAgent := mesh.URN("msg://agent/example/other")
	addAgent(t, f, otherAgent, other)
	r := request("shared")
	r.AgentURN = otherAgent
	second, err := f.s2.Admit(t.Context(), other, r)
	if err != nil {
		t.Fatal(err)
	}
	if second.Instance.ID == first.Instance.ID || second.Session.URN == first.Session.URN || second.OperationRef == first.OperationRef || second.Session.URN.Validate() != nil {
		t.Fatal("caller keys squat global identities", first, second)
	}
	if err := f.s2.ReleaseLease(t.Context(), other, authority(t, f, second), "urn:proof:quiesced"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []mesh.URN{first.Session.URN, "urn:session:absent"} {
		r.Key = "resume"
		r.SessionURN = target
		_, err = f.s2.Admit(t.Context(), other, r)
		if !errors.Is(err, ErrDenied) || err.Error() != ErrDenied.Error() {
			t.Fatal("resume existence oracle", err)
		}
	}
}
func TestAcquireExactExpiryBoundaryAndExpiredOutboxContents(t *testing.T) {
	f := setup(t)
	old := admitted(t, f, "old")
	f.c.Advance(time.Minute - time.Nanosecond)
	if _, err := f.s2.Admit(t.Context(), owner, request("new")); !errors.Is(err, ErrBound) {
		t.Fatal("lease not exclusive before expiry", err)
	}
	f.c.Advance(time.Nanosecond)
	current, err := f.s2.Admit(t.Context(), owner, request("new"))
	if err != nil || !current.Current {
		t.Fatal(current, err)
	}
	events, err := f.r1.Events(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	expired, acquired := false, false
	for _, event := range events {
		var lease mesh.BindingLease
		if err := json.Unmarshal(event.Payload, &lease); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "binding.expired":
			if lease.InstanceID != old.Instance.ID || lease.Holder != owner || lease.FencingToken != old.Lease.FencingToken || lease.AgentURN != agentURN || !event.CreatedAt.Equal(old.Lease.ExpiresAt) {
				t.Fatal("wrong expired event", event, lease)
			}
			expired = true
		case "binding.acquired":
			if lease.InstanceID == current.Instance.ID {
				if !sameLeaseIdentity(lease, current.Lease) || !lease.ExpiresAt.Equal(current.Lease.ExpiresAt) {
					t.Fatal("wrong acquire event", event, lease)
				}
				acquired = true
			}
		}
	}
	if !expired || !acquired {
		t.Fatal("binding evidence missing", expired, acquired)
	}
	retry, err := f.s1.Admit(t.Context(), owner, request("old"))
	if err != nil || retry.Current {
		t.Fatal("superseded reservation claims current", retry, err)
	}
}
func TestExpiredEventUsesCheckedClockAfterVerification(t *testing.T) {
	f := setup(t)
	old := admitted(t, f, "old")
	f.c.Advance(30 * time.Second)
	f.defs.hook = func() { f.c.Advance(45 * time.Second) }
	if _, err := f.s1.Admit(t.Context(), owner, request("new")); err != nil {
		t.Fatal(err)
	}
	events, err := f.r1.Events(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "binding.expired" {
			found = true
			if !e.CreatedAt.Equal(f.c.Now()) || e.CreatedAt.Before(old.Lease.ExpiresAt) {
				t.Fatal("expired event precedes actual expiry", e)
			}
		}
	}
	if !found {
		t.Fatal("no expired event")
	}
}
func TestReservationVersionsSurviveOwnRenewAndHostCanAdvance(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	a := authority(t, f, r)
	o := Observation{Authority: a, Status: mesh.InstanceRunning, SessionState: mesh.SessionRunning, SessionVersion: r.SessionVersion, InstanceVersion: r.InstanceVersion, ReportRef: "urn:report:ready"}
	f.c.Advance(time.Second)
	if _, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	ack, err := f.s1.Observe(t.Context(), owner, o)
	if err != nil || ack.SessionVersion != r.SessionVersion+1 || ack.InstanceVersion != r.InstanceVersion+1 {
		t.Fatal("host has no usable versions", ack, err)
	}
	if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:result"); err != nil {
		t.Fatal("own renewal invalidated report", err)
	}
	if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:result"); err != nil {
		t.Fatal("identical report not idempotent", err)
	}
	o.Status = mesh.InstanceWaiting
	o.Detail.Waiting = mesh.WaitingInput
	o.SessionVersion = ack.SessionVersion
	o.InstanceVersion = ack.InstanceVersion
	if _, err := f.s1.Observe(t.Context(), owner, o); err != nil {
		t.Fatal(err)
	}
	retry, err := f.s1.Admit(t.Context(), owner, request("first"))
	if err != nil || !retry.Current || retry.InstanceVersion != ack.InstanceVersion+1 {
		t.Fatal("renewed receipt lost current identity", retry, err)
	}
}
func TestResumeGuardsSessionCASPinAuthorizationAndReferences(t *testing.T) {
	for _, guard := range []string{"session version", "historical pin", "cross agent", "context", "store"} {
		t.Run(guard, func(t *testing.T) {
			f := setup(t)
			old := admitted(t, f, "old")
			if err := f.s1.ReleaseLease(t.Context(), owner, authority(t, f, old), "urn:proof:quiesced"); err != nil {
				t.Fatal(err)
			}
			r := request("resume")
			r.SessionURN = old.Session.URN
			r.ContextRef = old.Session.ContextRef
			r.StoreRef = old.Session.StoreRef
			want := ErrDenied
			switch guard {
			case "session version":
				want = fabricstore.ErrConflict
				f.defs.hook = func() {
					session, e := f.r2.Session(t.Context(), old.Session.URN)
					if e != nil {
						t.Fatal(e)
					}
					if e = f.r2.Write(t.Context(), func(tx *fabricstore.Tx) error { return tx.PutSession(session.Value, session.Version) }); e != nil {
						t.Fatal(e)
					}
				}
			case "historical pin":
				next := pin
				next.Revision = "r2"
				if err := f.r1.Write(t.Context(), func(tx *fabricstore.Tx) error {
					a, e := tx.Agent(agentURN)
					if e != nil {
						return e
					}
					if e = tx.AddDefinition(fabricstore.DefinitionRevision{Definition: next, SourceRef: "urn:source:r2"}); e != nil {
						return e
					}
					a.Value.Definition = next
					return tx.PutAgent(a.Value, a.Version)
				}); err != nil {
					t.Fatal(err)
				}
				f.s1.authorize = func(_ context.Context, a Authorization) error {
					if a.Definition == pin {
						return errors.New("historical pin refused")
					}
					return nil
				}
			case "cross agent":
				r.AgentURN = "msg://agent/example/another"
				addAgent(t, f, r.AgentURN, owner)
			case "context":
				r.ContextRef = "urn:context:wrong"
			case "store":
				r.StoreRef = "urn:store:wrong"
			}
			if _, err := f.s1.Admit(t.Context(), owner, r); !errors.Is(err, want) {
				t.Fatal("resume guard bypassed", guard, err)
			}
			if _, err := f.r2.Instance(t.Context(), executionID(owner, "resume")); !errors.Is(err, fabricstore.ErrNotFound) {
				t.Fatal("failed resume reserved instance", err)
			}
		})
	}
}
func TestRetiredHolderCanReleaseOrEndButCannotContinue(t *testing.T) {
	for _, finish := range []string{"release", "terminal"} {
		t.Run(finish, func(t *testing.T) {
			f := setup(t)
			r := admitted(t, f, "first")
			a := authority(t, f, r)
			retireAgent(t, f)
			if _, err := f.s1.Admit(t.Context(), owner, request("new")); !errors.Is(err, ErrDenied) {
				t.Fatal("retired admission allowed", err)
			}
			if _, err := f.s1.Admit(t.Context(), owner, request("first")); !errors.Is(err, ErrDenied) {
				t.Fatal("retired retry allowed", err)
			}
			if _, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute); !errors.Is(err, ErrDenied) {
				t.Fatal("retired renew allowed", err)
			}
			o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
			if err := observeErr(t.Context(), f.s1, owner, o); !errors.Is(err, ErrDenied) {
				t.Fatal("retired nonterminal allowed", err)
			}
			if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:result"); !errors.Is(err, ErrDenied) {
				t.Fatal("retired report allowed", err)
			}
			if finish == "release" {
				if err := f.s1.ReleaseLease(t.Context(), owner, a, "urn:proof:quiesced"); err != nil {
					t.Fatal("retired holder cannot release", err)
				}
			} else {
				o.Status = mesh.InstanceStopped
				o.Detail.Stopped = mesh.StopCanceled
				o.SessionState = mesh.SessionEnded
				if err := observeErr(t.Context(), f.s1, owner, o); err != nil {
					t.Fatal("retired holder cannot terminate", err)
				}
			}
			head, err := f.r1.BindingHead(t.Context(), agentURN)
			if err != nil || head.Value.Lease != nil {
				t.Fatal("retired lease stranded", head, err)
			}
		})
	}
}
func TestBoundedValidatorContextOpacityAndReportSecondHeldCheck(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	a := authority(t, f, r)
	f.s1.validate = func(ctx context.Context, _ mesh.URN, _ Observation) error {
		<-ctx.Done()
		return fmt.Errorf("private host: %w", ctx.Err())
	}
	f.db.SetMaxOpenConns(1)
	began := time.Now()
	err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:blocked")
	requireBareError(t, err, context.DeadlineExceeded)
	if time.Since(began) > time.Second {
		t.Fatal("validator hangs writer or leaks cause", err, time.Since(began))
	}
	for _, fault := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private failure")} {
		f.s1.validate = func(context.Context, mesh.URN, Observation) error { return fmt.Errorf("private host: %w", fault) }
		err = f.s1.ReportReference(t.Context(), owner, a, "urn:report:refused")
		if errors.Is(fault, context.Canceled) || errors.Is(fault, context.DeadlineExceeded) {
			requireBareError(t, err, fault)
		} else {
			requireBareError(t, err, ErrDenied)
		}
	}
	for _, fault := range []error{context.Canceled, context.DeadlineExceeded} {
		f.s1.authorize = func(context.Context, Authorization) error { return fmt.Errorf("private host: %w", fault) }
		requireBareError(t, f.s1.ReportReference(t.Context(), owner, a, "urn:report:authorizer-context"), fault)
	}
	f.s1.authorize = func(context.Context, Authorization) error { return nil }
	f.s1.validate = func(context.Context, mesh.URN, Observation) error { f.c.Advance(time.Minute); return nil }
	if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:expired-during-validation"); !errors.Is(err, ErrStale) {
		t.Fatal("report did not recheck expiry", err)
	}
	events, err := f.r1.Events(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "instance.report" {
			t.Fatal("failed report persisted", e)
		}
	}
}
func TestNonterminalOutboxRollbackAndFailedReportLeavesNoEvent(t *testing.T) {
	for _, action := range []Action{Lifecycle, Report} {
		t.Run(string(action), func(t *testing.T) {
			f := setup(t)
			r := admitted(t, f, "first")
			before, err := f.r1.Snapshot(t.Context(), agentURN, r.Session.URN)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.db.ExecContext(t.Context(), `CREATE TRIGGER refuse_outbox BEFORE INSERT ON fabric_outbox BEGIN SELECT RAISE(ABORT,'fixture event fault'); END`); err != nil {
				t.Fatal(err)
			}
			if action == Lifecycle {
				err = observeErr(t.Context(), f.s1, owner, observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning))
			} else {
				err = f.s1.ReportReference(t.Context(), owner, authority(t, f, r), "urn:report:failed")
			}
			if err == nil {
				t.Fatal("event fault swallowed")
			}
			after, err := f.r2.Snapshot(t.Context(), agentURN, r.Session.URN)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("nonterminal write survived rollback", after, err)
			}
			events, err := f.r2.Events(t.Context(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Type == "instance.observed" || e.Type == "instance.report" {
					t.Fatal("failed event persisted", e)
				}
			}
		})
	}
}
func TestInstanceTransitionsAndTerminalSessionCombinations(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	for _, step := range []struct {
		status mesh.InstanceStatus
		detail mesh.InstanceDetail
	}{{mesh.InstanceRunning, mesh.InstanceDetail{}}, {mesh.InstanceWaiting, mesh.InstanceDetail{Waiting: mesh.WaitingAuth}}, {mesh.InstanceRunning, mesh.InstanceDetail{}}, {mesh.InstanceStopping, mesh.InstanceDetail{}}} {
		if err := observeErr(t.Context(), f.s1, owner, observation(t, f, r, step.status, step.detail, mesh.SessionRunning)); err != nil {
			t.Fatal(err)
		}
	}
	if err := observeErr(t.Context(), f.s1, owner, observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("stopping regressed to running", err)
	}
	if err := observeErr(t.Context(), f.s1, owner, observation(t, f, r, mesh.InstanceRejected, mesh.InstanceDetail{}, mesh.SessionEnded)); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("active execution rejected as never started", err)
	}
	for _, status := range []mesh.InstanceStatus{mesh.InstanceRunning, mesh.InstanceStopped, mesh.InstanceRejected} {
		for _, state := range []mesh.SessionState{mesh.SessionStarting, mesh.SessionRunning, mesh.SessionPaused, mesh.SessionDetached, mesh.SessionOrphaned, mesh.SessionEnded} {
			t.Run(string(status)+"/"+string(state), func(t *testing.T) {
				f := setup(t)
				r := admitted(t, f, "combination")
				detail := mesh.InstanceDetail{}
				if status == mesh.InstanceStopped {
					detail.Stopped = mesh.StopFailed
				}
				err := observeErr(t.Context(), f.s1, owner, observation(t, f, r, status, detail, state))
				valid := (!terminal(status) && state != mesh.SessionEnded) || (terminal(status) && (state == mesh.SessionEnded || state == mesh.SessionOrphaned))
				if valid && err != nil || !valid && !errors.Is(err, fabricstore.ErrInvalid) {
					t.Fatal("state combination", status, state, err)
				}
				head, e := f.r1.BindingHead(t.Context(), agentURN)
				if e != nil {
					t.Fatal(e)
				}
				if valid && terminal(status) {
					if head.Value.Lease != nil {
						t.Fatal("terminal did not release")
					}
				} else {
					if head.Value.Lease == nil {
						t.Fatal("unresolved/invalid observation released")
					}
				}
			})
		}
	}
}
func TestTTLBoundsAndConsumedAdmissionTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -1, time.Hour + 1} {
		f := setup(t)
		r := request("invalid")
		r.TTL = ttl
		if _, err := f.s1.Admit(t.Context(), owner, r); !errors.Is(err, fabricstore.ErrInvalid) {
			t.Fatal("admit TTL allowed", ttl, err)
		}
		valid := admitted(t, f, "valid")
		if _, err := f.s1.RenewLease(t.Context(), owner, authority(t, f, valid), ttl); !errors.Is(err, fabricstore.ErrInvalid) {
			t.Fatal("renew TTL allowed", ttl, err)
		}
	}
	f := setup(t)
	r := request("maximum")
	r.TTL = time.Hour
	if _, err := f.s1.Admit(t.Context(), owner, r); err != nil {
		t.Fatal("max TTL refused", err)
	}
	f = setup(t)
	f.defs.hook = func() { f.c.Advance(time.Minute) }
	if _, err := f.s1.Admit(t.Context(), owner, request("consumed")); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("consumed TTL misclassified", err)
	}
	if _, err := f.r1.Admission(t.Context(), owner, "consumed"); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("consumed TTL persisted", err)
	}
}

func requireBareError(t *testing.T, err, expected error) {
	t.Helper()
	//nolint:errorlint // Exact sentinel identity prevents private wrapper text leaking.
	if err != expected {
		t.Fatal("context result is not the bare sentinel", err)
	}
}

func TestCurrentBetweenReceiptExpiryAndRenewedHeadExpiry(t *testing.T) {
	f := setup(t)
	r := admitted(t, f, "first")
	f.c.Advance(time.Second)
	if _, err := f.s1.RenewLease(t.Context(), owner, authority(t, f, r), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	f.c.Advance(time.Minute)
	retry, err := f.s1.Admit(t.Context(), owner, request("first"))
	if err != nil || !retry.Current || retry.Lease != r.Lease || !f.c.Now().After(retry.Lease.ExpiresAt) {
		t.Fatal("renewed head authority incorrectly depends on receipt expiry", retry, err)
	}
}

func TestLateValidatorRefusedWithoutBlockingIndependentWriter(t *testing.T) {
	for _, action := range []Action{Lifecycle, Report} {
		t.Run(string(action), func(t *testing.T) {
			f := setup(t)
			r := admitted(t, f, "first")
			o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
			entered, finish := make(chan struct{}), make(chan struct{})
			defer close(finish)
			f.s1.validate = func(context.Context, mesh.URN, Observation) error {
				close(entered)
				<-finish // deliberately ignores cancellation
				return nil
			}
			done := make(chan error, 1)
			go func() {
				if action == Lifecycle {
					done <- observeErr(t.Context(), f.s1, owner, o)
				} else {
					done <- f.s1.ReportReference(t.Context(), owner, o.Authority, o.ReportRef)
				}
			}()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if err := f.r2.Write(ctx, func(*fabricstore.Tx) error { return nil }); err != nil {
				t.Fatal("validator held SQLite writer", err)
			}
			// Release the late callback after its own deadline, then require refusal.
			time.Sleep(150 * time.Millisecond)
			finish <- struct{}{}
			requireBareError(t, <-done, context.DeadlineExceeded)
			current, err := f.r2.Instance(t.Context(), r.Instance.ID)
			if err != nil || current.Version != r.InstanceVersion {
				t.Fatal("late validator changed state", current, err)
			}
			called := false
			f.s1.validate = func(context.Context, mesh.URN, Observation) error { called = true; return nil }
			f.c.Advance(time.Minute)
			if err := f.s1.ReportReference(t.Context(), owner, o.Authority, "urn:report:stale"); !errors.Is(err, ErrStale) || called {
				t.Fatal("stale authority reached validator", called, err)
			}
		})
	}
}

func TestEveryWriterOutboxContract(t *testing.T) {
	for _, kind := range []string{"acquired", "renewed", "released", "expired", "observed", "report", "terminal released"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			r := admitted(t, f, "first")
			a := authority(t, f, r)
			id, eventType, bindingKind := r.OperationRef+"/acquired", "binding.acquired", "acquired"
			switch kind {
			case "acquired":
			case "renewed":
				f.c.Advance(time.Second)
				lease, err := f.s1.RenewLease(t.Context(), owner, a, 2*time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				id, _ = digest([]any{a.Fence, a.AgentURN, lease.ExpiresAt, "renewed"})
				eventType, bindingKind = "binding.renewed", "renewed"
			case "released":
				if err := f.s1.ReleaseLease(t.Context(), owner, a, "urn:proof:quiesced"); err != nil {
					t.Fatal(err)
				}
				id, _ = digest([]any{a, "released"})
				eventType, bindingKind = "binding.released", "released"
			case "expired":
				f.c.Advance(time.Minute)
				next := admitted(t, f, "second")
				id, eventType, bindingKind = next.OperationRef+"/expired", "binding.expired", "expired"
			case "observed", "terminal released":
				o := observation(t, f, r, mesh.InstanceRunning, mesh.InstanceDetail{}, mesh.SessionRunning)
				if kind == "terminal released" {
					o.Status, o.Detail.Stopped, o.SessionState = mesh.InstanceStopped, mesh.StopCanceled, mesh.SessionEnded
				}
				if _, err := f.s1.Observe(t.Context(), owner, o); err != nil {
					t.Fatal(err)
				}
				id, _ = digest([]any{a, o.InstanceVersion, "observed"})
				eventType, bindingKind = "instance.observed", ""
				if kind == "terminal released" {
					id += "/released"
					eventType, bindingKind = "binding.released", "released"
				}
			case "report":
				if err := f.s1.ReportReference(t.Context(), owner, a, "urn:report:result"); err != nil {
					t.Fatal(err)
				}
				id, _ = digest([]any{a, "urn:report:result", "report"})
				eventType, bindingKind = "instance.report", ""
			}
			if err := f.r2.Write(t.Context(), func(tx *fabricstore.Tx) error {
				e, err := tx.Event(id)
				if err != nil {
					return err
				}
				if e.Type != eventType || e.AggregateURN != agentURN {
					t.Fatalf("outbox contract: got %s %s, want %s %s", e.Type, e.AggregateURN, eventType, agentURN)
				}
				if bindingKind != "" {
					b, err := tx.BindingEvent(id)
					if err != nil {
						return err
					}
					if b.Value.Kind != bindingKind || b.Value.Lease.AgentURN != a.AgentURN || b.Value.Lease.SessionURN != a.SessionURN || b.Value.Lease.InstanceID != a.InstanceID || b.Value.Lease.FencingToken != a.Fence {
						t.Fatalf("binding history contract: %+v", b)
					}
					var payload mesh.BindingLease
					if err := json.Unmarshal(e.Payload, &payload); err != nil {
						return err
					}
					if payload != b.Value.Lease {
						t.Fatalf("outbox lease differs from durable history: %+v %+v", payload, b.Value.Lease)
					}
				} else {
					var payload Observation
					if err := json.Unmarshal(e.Payload, &payload); err != nil {
						return err
					}
					if payload.Authority != a || !textOK(payload.ReportRef) {
						t.Fatalf("outbox observation identity: %+v", payload)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
