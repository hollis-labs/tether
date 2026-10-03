package fabricstore

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/store"
)

var testPin = mesh.DefinitionRef{ID: "assistant", Revision: "r1", Digest: "fixture-content-digest"}

const testAgent mesh.URN = "msg://agent/example/assistant"
const testOwner mesh.URN = "msg://user/example/operator"
const testSession mesh.URN = "urn:session:example"

var testTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func openRepository(t *testing.T) (*Repository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s.DB()), path
}
func seed(t *testing.T, r *Repository) {
	t.Helper()
	err := r.Write(context.Background(), func(tx *Tx) error {
		if err := tx.AddDefinition(DefinitionRevision{Definition: testPin, SourceRef: "urn:source:definition"}); err != nil {
			return err
		}
		if err := tx.PutActor(Actor{Actor: mesh.Actor{URN: testAgent, Kind: mesh.ActorAgent}, Owner: testOwner, Lifecycle: mesh.EnrollmentActive}, 0); err != nil {
			return err
		}
		if err := tx.PutAgent(mesh.Agent{URN: testAgent, Owner: testOwner, Definition: testPin, Lifecycle: mesh.EnrollmentActive}, 0); err != nil {
			return err
		}
		return tx.PutSession(mesh.Session{URN: testSession, AgentURN: testAgent, Definition: testPin, ContextRef: "urn:context:example", StoreRef: "urn:store:example", State: mesh.SessionStarting, CreatedAt: testTime, UpdatedAt: testTime}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func instance(id string, fence uint64) mesh.AgentInstance {
	return mesh.AgentInstance{ID: id, AgentURN: testAgent, Definition: testPin, NodeRef: "urn:node:example", RuntimeRef: "urn:runtime:example", SessionURN: testSession, BindingFence: fence, LaunchRecordRef: "urn:launch:example", Status: mesh.InstanceStarting, CreatedAt: testTime, UpdatedAt: testTime}
}
func lease(id string, fence uint64) *mesh.BindingLease {
	return &mesh.BindingLease{AgentURN: testAgent, SessionURN: testSession, InstanceID: id, Holder: testOwner, ExpiresAt: testTime.Add(time.Hour), FencingToken: fence}
}
func TestTransactionRollbackAndReferentialChecks(t *testing.T) {
	r, _ := openRepository(t)
	seed(t, r)
	ctx := context.Background()
	abort := errors.New("abort")
	err := r.Write(ctx, func(tx *Tx) error {
		if err := tx.PutInstance(instance("aborted", 0), 0); err != nil {
			return err
		}
		if err := tx.AppendEvent(Event{ID: "aborted-event", AggregateURN: testAgent, Type: "instance.created", Payload: []byte(`{}`), CreatedAt: testTime}); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if _, err := r.Instance(ctx, "aborted"); !errors.Is(err, ErrNotFound) {
		t.Fatal("instance escaped rollback", err)
	}
	events, err := r.Events(ctx, 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatal("outbox escaped rollback", events, err)
	}
	bad := instance("orphan", 0)
	bad.SessionURN = "urn:session:missing"
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutInstance(bad, 0) }); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan accepted", err)
	}
	bad = instance("wrong-pin", 0)
	bad.Definition.Revision = "other"
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutInstance(bad, 0) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("pin mismatch accepted", err)
	}
	actor, err := r.Actor(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	actor.Value.Lifecycle = mesh.EnrollmentRetired
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutActor(actor.Value, actor.Version) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("actor/agent divergence accepted", err)
	}
}
func TestPinnedSessionSurvivesAgentRebind(t *testing.T) {
	r, _ := openRepository(t)
	seed(t, r)
	ctx := context.Background()
	agent, err := r.Agent(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	next := testPin
	next.Revision = "r2"
	next.Digest = "second-content-digest"
	agent.Value.Definition = next
	if err := r.Write(ctx, func(tx *Tx) error {
		if err := tx.AddDefinition(DefinitionRevision{Definition: next, SourceRef: "urn:source:second"}); err != nil {
			return err
		}
		return tx.PutAgent(agent.Value, agent.Version)
	}); err != nil {
		t.Fatal(err)
	}
	session, err := r.Session(ctx, testSession)
	if err != nil || session.Value.Definition != testPin {
		t.Fatal("session changed beneath rebind", session, err)
	}
	session.Value.Definition = next
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutSession(session.Value, session.Version) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("session rebind accepted", err)
	}
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutInstance(instance("continuation", 0), 0) }); err != nil {
		t.Fatal(err)
	}
}
func TestTwoConnectionsVersionAndFence(t *testing.T) {
	r, path := openRepository(t)
	seed(t, r)
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other := New(second.DB())
	ctx := context.Background()
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutBindingHead(BindingHead{AgentURN: testAgent}, 0) }); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, repo := range []*Repository{r, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := []string{"first", "second"}[i]
			results <- repo.Write(ctx, func(tx *Tx) error {
				if err := tx.PutInstance(instance(id, 1), 0); err != nil {
					return err
				}
				return tx.PutBindingHead(BindingHead{AgentURN: testAgent, HighWater: 1, Lease: lease(id, 1)}, 1)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	head, err := r.BindingHead(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	winner := head.Value.Lease.InstanceID
	loser := "first"
	if winner == loser {
		loser = "second"
	}
	if _, err := r.Instance(ctx, loser); !errors.Is(err, ErrNotFound) {
		t.Fatal("losing transaction did not roll back", err)
	}
	head.Value.Lease = nil
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutBindingHead(head.Value, head.Version) }); err != nil {
		t.Fatal(err)
	}
	head, err = other.BindingHead(ctx, testAgent)
	if err != nil || head.Value.HighWater != 1 || head.Value.Lease != nil {
		t.Fatal(head, err)
	}
	if err := other.Write(ctx, func(tx *Tx) error {
		return tx.PutBindingHead(BindingHead{AgentURN: testAgent, HighWater: 1, Lease: lease(winner, 1)}, head.Version)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatal("released fence reused", err)
	}
	if err := other.Write(ctx, func(tx *Tx) error {
		return tx.PutBindingHead(BindingHead{AgentURN: testAgent, HighWater: uint64(math.MaxInt64) + 1}, head.Version)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatal("overflow accepted", err)
	}
	if err := other.Write(ctx, func(tx *Tx) error {
		if err := tx.PutInstance(instance("next", 2), 0); err != nil {
			return err
		}
		if err := tx.PutBindingHead(BindingHead{AgentURN: testAgent, HighWater: 2, Lease: lease("next", 2)}, head.Version); err != nil {
			return err
		}
		return tx.AddBindingEvent(BindingEvent{ID: "acquired", Lease: *lease("next", 2), Kind: "acquired", At: testTime})
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.Snapshot(ctx, testAgent, testSession)
	if err != nil || snapshot.Head.Value.HighWater != 2 || snapshot.Instance.Value.ID != "next" {
		t.Fatal(snapshot, err)
	}
}
func TestProvenanceAdmissionAndOutbox(t *testing.T) {
	r, _ := openRepository(t)
	seed(t, r)
	ctx := context.Background()
	artifact := DefinitionArtifact{Definition: testPin, Digest: "artifact-digest", SourceRef: "urn:source:artifact", VerificationRef: "urn:verification:example"}
	candidate := MigrationCandidate{ID: "candidate", LegacyRef: "urn:legacy:example", EvidenceRef: "urn:evidence:example", Reason: "owner verified", State: "reviewed"}
	receipt := ImportReceipt{ID: "receipt", CandidateID: candidate.ID, ActorURN: testAgent, ApprovalRef: "urn:approval:example", At: testTime}
	legacy := LegacyRef{Source: "registry", Key: string(testAgent), ActorURN: testAgent, ReceiptID: receipt.ID}
	admission := Admission{Caller: testOwner, Key: "launch-key", RequestDigest: "request-digest", OperationRef: "urn:operation:example", State: "reserved"}
	err := r.Write(ctx, func(tx *Tx) error {
		for _, op := range []func() error{
			func() error { return tx.AddArtifact(artifact) }, func() error { return tx.PutCandidate(candidate, 0) }, func() error { return tx.AddReceipt(receipt) }, func() error { return tx.AddLegacyRef(legacy) }, func() error { return tx.PutAdmission(admission, 0) }, func() error {
				return tx.AppendEvent(Event{ID: "event", AggregateURN: testAgent, Type: "agent.enrolled", Payload: []byte(`{"version":1}`), CreatedAt: testTime})
			},
		} {
			if err := op(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := r.Artifact(ctx, testPin, artifact.Digest)
	if err != nil || !reflect.DeepEqual(actual.Value, artifact) {
		t.Fatal(actual, err)
	}
	gotCandidate, err := r.Candidate(ctx, candidate.ID)
	if err != nil || gotCandidate.Value != candidate {
		t.Fatal(gotCandidate, err)
	}
	gotReceipt, err := r.Receipt(ctx, receipt.ID)
	if err != nil || gotReceipt.Value != receipt {
		t.Fatal(gotReceipt, err)
	}
	gotLegacy, err := r.LegacyRef(ctx, legacy.Source, legacy.Key)
	if err != nil || gotLegacy.Value != legacy {
		t.Fatal(gotLegacy, err)
	}
	gotAdmission, err := r.Admission(ctx, admission.Caller, admission.Key)
	if err != nil {
		t.Fatal(err)
	}
	admission.RequestDigest = "different"
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutAdmission(admission, gotAdmission.Version) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("idempotency request changed", err)
	}
	if err := r.Write(ctx, func(tx *Tx) error { return tx.AddArtifact(artifact) }); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable artifact overwritten", err)
	}
	events, err := r.Events(ctx, 0, 1)
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	cursor := events[0].Cursor
	if err := r.Write(ctx, func(tx *Tx) error { return tx.MarkDelivered(cursor, testTime.Add(time.Second)) }); err != nil {
		t.Fatal(err)
	}
	events, err = r.Events(ctx, 0, 1)
	if err != nil || events[0].DeliveredAt == nil {
		t.Fatal(events, err)
	}
	events, err = r.Events(ctx, cursor, 1)
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
}

func TestTwoConnectionsAdmissionAndDurableReopen(t *testing.T) {
	r, path := openRepository(t)
	seed(t, r)
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other := New(second.DB())
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, repo := range []*Repository{r, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := []string{"request-a", "request-b"}[i]
			results <- repo.Write(ctx, func(tx *Tx) error {
				if err := tx.PutAdmission(Admission{Caller: testOwner, Key: "same-key", RequestDigest: id, OperationRef: "urn:operation:" + id, State: "reserved"}, 0); err != nil {
					return err
				}
				return tx.AppendEvent(Event{ID: id, AggregateURN: testAgent, Type: "admission.reserved", Payload: []byte(`{}`), CreatedAt: testTime})
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	durable := New(reopened.DB())
	admission, err := durable.Admission(ctx, testOwner, "same-key")
	if err != nil || admission.Version != 1 {
		t.Fatal(admission, err)
	}
	events, err := durable.Events(ctx, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != admission.Value.RequestDigest {
		t.Fatal("losing admission event escaped", events, err)
	}
	session, err := durable.Session(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	session.Value.State = mesh.SessionRunning
	if err := durable.Write(ctx, func(tx *Tx) error { return tx.PutSession(session.Value, session.Version) }); err != nil {
		t.Fatal(err)
	}
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutSession(session.Value, session.Version) }); !errors.Is(err, ErrConflict) {
		t.Fatal("stale session update accepted", err)
	}
}

func TestCanceledTransactionReleasesWriter(t *testing.T) {
	r, _ := openRepository(t)
	seed(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := r.Write(ctx, func(tx *Tx) error {
		if err := tx.PutInstance(instance("canceled", 0), 0); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.Instance(context.Background(), "canceled"); !errors.Is(err, ErrNotFound) {
		t.Fatal("canceled transaction committed", err)
	}
	if err := r.Write(context.Background(), func(tx *Tx) error { return tx.PutInstance(instance("later", 0), 0) }); err != nil {
		t.Fatal("writer not released", err)
	}
}

func TestNonAgentAndAtomicRetirement(t *testing.T) {
	r, _ := openRepository(t)
	seed(t, r)
	ctx := context.Background()
	service := Actor{Actor: mesh.Actor{URN: "msg://service/example/indexer", Kind: mesh.ActorService}, Owner: testOwner, Lifecycle: mesh.EnrollmentActive}
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutActor(service, 0) }); err != nil {
		t.Fatal(err)
	}
	if err := r.Write(ctx, func(tx *Tx) error {
		return tx.PutAgent(mesh.Agent{URN: service.URN, Owner: testOwner, Definition: testPin, Lifecycle: mesh.EnrollmentActive}, 0)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatal("service gained fabricated agent", err)
	}
	actor, err := r.Actor(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := r.Agent(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	actor.Value.Lifecycle = mesh.EnrollmentRetired
	agent.Value.Lifecycle = mesh.EnrollmentRetired
	if err := r.Write(ctx, func(tx *Tx) error {
		if err := tx.PutActor(actor.Value, actor.Version); err != nil {
			return err
		}
		return tx.PutAgent(agent.Value, agent.Version)
	}); err != nil {
		t.Fatal(err)
	}
	actor, err = r.Actor(ctx, testAgent)
	if err != nil {
		t.Fatal(err)
	}
	actor.Value.Lifecycle = mesh.EnrollmentActive
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutActor(actor.Value, actor.Version) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired identity reactivated", err)
	}
	session := mesh.Session{URN: "urn:session:new", AgentURN: testAgent, Definition: testPin, ContextRef: "urn:context:new", StoreRef: "urn:store:new", State: mesh.SessionStarting, CreatedAt: testTime, UpdatedAt: testTime}
	if err := r.Write(ctx, func(tx *Tx) error { return tx.PutSession(session, 0) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired agent created session", err)
	}
}
