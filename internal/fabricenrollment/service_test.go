package fabricenrollment

import (
	"context"
	"encoding/json"
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
const actorURN mesh.URN = "msg://agent/example/Original-Identity"

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var pin = mesh.DefinitionRef{ID: "assistant", Revision: "r1", Digest: "sha256:" + strings.Repeat("a", 64)}
var nextPin = mesh.DefinitionRef{ID: "assistant", Revision: "r2", Digest: "sha256:" + strings.Repeat("b", 64)}

type fakeDefinitions struct {
	mu     sync.Mutex
	values map[mesh.DefinitionRef]definitionresolve.VerifiedDefinition
	reads  int
	hook   func()
}

func (f *fakeDefinitions) Load(ctx context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	if err := ctx.Err(); err != nil {
		return definitionresolve.VerifiedDefinition{}, err
	}
	f.mu.Lock()
	f.reads++
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	value, ok := f.values[p]
	if !ok {
		return value, definitionresolve.ErrPinMismatch
	}
	return value, nil
}
func harness(t *testing.T) (*Service, *fabricstore.Repository, *fakeDefinitions) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := fabricstore.New(db.DB())
	defs := &fakeDefinitions{values: map[mesh.DefinitionRef]definitionresolve.VerifiedDefinition{}}
	for _, p := range []mesh.DefinitionRef{pin, nextPin} {
		defs.values[p] = definitionresolve.VerifiedDefinition{Pin: p, Definition: &agentdef.Definition{Capabilities: []agentdef.Capability{{ID: "review", Description: "Review source"}, {ID: "edit", Description: "Edit source"}}, Body: "private instructions", HarnessProfile: agentdef.HarnessProfile{Permissions: agentdef.PermissionProfile{Profile: "private-policy"}}}}
		if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error {
			return tx.AddDefinition(fabricstore.DefinitionRevision{Definition: p, SourceRef: "urn:source:" + p.Revision})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service, err := New(repo, defs, func(_ context.Context, a Authorization) error {
		if a.Caller != a.Owner {
			return ErrDenied
		}
		return nil
	}, func(_ context.Context, _ Authorization, _ definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true, Capabilities: []string{"review"}}, nil
	}, func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, defs
}
func request(urn mesh.URN) Enrollment {
	return Enrollment{Actor: mesh.Actor{URN: urn, Kind: mesh.ActorAgent}, Owner: owner, Definition: &pin}
}
func enroll(t *testing.T, s *Service, urn mesh.URN) {
	t.Helper()
	if err := s.EnrollActor(t.Context(), owner, request(urn)); err != nil {
		t.Fatal(err)
	}
}
func requireNotEnrolled(t *testing.T, r *fabricstore.Repository, urn mesh.URN) {
	t.Helper()
	if _, err := r.Actor(t.Context(), urn); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("unexpected enrollment", err)
	}
}
func bind(t *testing.T, r *fabricstore.Repository, expires time.Time) {
	t.Helper()
	err := r.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if err := tx.PutSession(mesh.Session{URN: "urn:session:fixture", AgentURN: actorURN, Definition: pin, ContextRef: "urn:context:fixture", StoreRef: "urn:store:fixture", State: mesh.SessionPaused, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			return err
		}
		if err := tx.PutInstance(mesh.AgentInstance{ID: "fixture-instance", AgentURN: actorURN, Definition: pin, SessionURN: "urn:session:fixture", NodeRef: "urn:node:fixture", RuntimeRef: "urn:runtime:fixture", BindingFence: 1, LaunchRecordRef: "urn:launch:fixture", Status: mesh.InstanceWaiting, Detail: mesh.InstanceDetail{Waiting: mesh.WaitingApproval}, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			return err
		}
		return tx.PutBindingHead(fabricstore.BindingHead{AgentURN: actorURN, HighWater: 1, Lease: &mesh.BindingLease{AgentURN: actorURN, SessionURN: "urn:session:fixture", InstanceID: "fixture-instance", Holder: owner, ExpiresAt: expires, FencingToken: 1}}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestEnrollChecksOwnerKindPinsAndNeverMergesIdentity(t *testing.T) {
	s, r, defs := harness(t)
	if err := s.EnrollActor(t.Context(), "msg://user/example/stranger", request(actorURN)); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if defs.reads != 0 {
		t.Fatal("unauthorized caller read definition")
	}
	requireNotEnrolled(t, r, actorURN)
	bad := request(actorURN)
	bad.Actor.Kind = mesh.ActorService
	if err := s.EnrollActor(t.Context(), owner, bad); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal(err)
	}
	bad = request(actorURN)
	unknown := pin
	unknown.Revision = "missing"
	bad.Definition = &unknown
	if err := s.EnrollActor(t.Context(), owner, bad); !errors.Is(err, definitionresolve.ErrPinMismatch) {
		t.Fatal(err)
	}
	requireNotEnrolled(t, r, actorURN)
	enroll(t, s, actorURN)
	got, err := r.Agent(t.Context(), actorURN)
	if err != nil || got.Value.URN != actorURN || got.Value.Definition != pin {
		t.Fatal(got, err)
	}
	if err := s.EnrollActor(t.Context(), owner, request(actorURN)); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("duplicate identity merged", err)
	}
	for _, kind := range []mesh.ActorKind{mesh.ActorUser, mesh.ActorService, mesh.ActorTool} {
		urn := mesh.URN("msg://" + string(kind) + "/example/participant")
		if err := s.EnrollActor(t.Context(), owner, Enrollment{Actor: mesh.Actor{URN: urn, Kind: kind}, Owner: owner}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Agent(t.Context(), urn); !errors.Is(err, fabricstore.ErrNotFound) {
			t.Fatal("nonagent got executable record", err)
		}
	}
	events, err := r.Events(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.AggregateURN == actorURN && event.Type == "enrollment.enroll" {
			found = true
			if strings.Contains(string(event.Payload), "private-") {
				t.Fatal("event contains operational content")
			}
		}
	}
	if !found {
		t.Fatal("missing committed enrollment event")
	}
}
func TestRebindRequiresReleasedBindingAndLeavesHistoricalPin(t *testing.T) {
	for _, expiry := range []time.Time{now.Add(time.Minute), now.Add(-time.Minute)} {
		t.Run(expiry.Format(time.RFC3339), func(t *testing.T) {
			s, r, _ := harness(t)
			enroll(t, s, actorURN)
			bind(t, r, expiry)
			if err := s.RebindAgent(t.Context(), owner, actorURN, nextPin, 1); !errors.Is(err, ErrBound) {
				t.Fatal(err)
			}
			if err := s.RetireActor(t.Context(), owner, actorURN, 1); !errors.Is(err, ErrBound) {
				t.Fatal(err)
			}
			head, err := r.BindingHead(t.Context(), actorURN)
			if err != nil {
				t.Fatal(err)
			}
			head.Value.Lease = nil
			if err := r.Write(t.Context(), func(tx *fabricstore.Tx) error { return tx.PutBindingHead(head.Value, head.Version) }); err != nil {
				t.Fatal(err)
			}
			if err := s.RebindAgent(t.Context(), owner, actorURN, nextPin, 1); err != nil {
				t.Fatal(err)
			}
			historical, err := r.Session(t.Context(), "urn:session:fixture")
			if err != nil || historical.Value.Definition != pin {
				t.Fatal(historical, err)
			}
			current, err := r.Agent(t.Context(), actorURN)
			if err != nil || current.Value.Definition != nextPin {
				t.Fatal(current, err)
			}
			if err := s.RebindAgent(t.Context(), owner, actorURN, pin, 1); !errors.Is(err, fabricstore.ErrConflict) {
				t.Fatal("stale rebind succeeded", err)
			}
			if err := s.RetireActor(t.Context(), owner, actorURN, 1); err != nil {
				t.Fatal(err)
			}
			actor, err := r.Actor(t.Context(), actorURN)
			if err != nil || actor.Value.Lifecycle != mesh.EnrollmentRetired {
				t.Fatal(actor, err)
			}
			current, err = r.Agent(t.Context(), actorURN)
			if err != nil || current.Value.Lifecycle != mesh.EnrollmentRetired {
				t.Fatal(current, err)
			}
			if err := s.RebindAgent(t.Context(), owner, actorURN, pin, current.Version); !errors.Is(err, ErrRetired) {
				t.Fatal(err)
			}
			if err := s.EnrollActor(t.Context(), owner, request(actorURN)); !errors.Is(err, fabricstore.ErrConflict) {
				t.Fatal("retired identity recreated", err)
			}
		})
	}
}
func TestEnrollmentMutationDuringPinVerificationRefusesRebind(t *testing.T) {
	s, r, defs := harness(t)
	enroll(t, s, actorURN)
	defs.hook = func() {
		defs.hook = nil
		agent, err := r.Agent(t.Context(), actorURN)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Write(t.Context(), func(tx *fabricstore.Tx) error { return tx.PutAgent(agent.Value, agent.Version) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RebindAgent(t.Context(), owner, actorURN, nextPin, 1); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal(err)
	}
	got, err := r.Agent(t.Context(), actorURN)
	if err != nil || got.Value.Definition != pin {
		t.Fatal(got, err)
	}
}
func TestOutboxFailureRollsBackEnrollment(t *testing.T) {
	s, r, _ := harness(t)
	s.now = func() time.Time { return time.Time{} }
	if err := s.EnrollActor(t.Context(), owner, request(actorURN)); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal(err)
	}
	requireNotEnrolled(t, r, actorURN)
}
func TestDirectoryPublicationAuthorizationRedactionAndConflict(t *testing.T) {
	s, _, defs := harness(t)
	enroll(t, s, actorURN)
	hidden := mesh.URN("msg://agent/example/unpublished")
	enroll(t, s, hidden)
	s.advertise = func(_ context.Context, a Authorization, _ definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: a.Target == actorURN, Capabilities: []string{"review"}}, nil
	}
	page, err := s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	visible := map[mesh.URN]DirectoryRecord{}
	for _, record := range page.Records {
		visible[record.URN] = record
	}
	if _, found := visible[hidden]; found {
		t.Fatal("enrollment implied publication")
	}
	record, found := visible[actorURN]
	if !found || record.Definition == nil || *record.Definition != pin || !reflect.DeepEqual(record.Capabilities, []string{"review"}) || !record.ValidUntil.Equal(now.Add(time.Minute)) {
		t.Fatal(record)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{string(owner), "private instructions", "private-policy", "source_ref", "runtime_ref", "owner"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("directory leaked", secret)
		}
	}
	reads := defs.reads
	if _, err := s.Directory(t.Context(), "msg://user/example/stranger", owner, "", 100); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if reads != defs.reads {
		t.Fatal("unauthorized directory read content")
	}
	s.advertise = func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true, Capabilities: []string{"unoffered"}}, nil
	}
	filtered, err := s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range filtered.Records {
		for _, id := range row.Capabilities {
			if id == "unoffered" {
				t.Fatal("invented capability published")
			}
		}
	}
	s.advertise = func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true}, nil
	}
	defs.hook = func() {
		defs.hook = nil
		if err := s.RetireActor(t.Context(), owner, actorURN, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("mixed directory versions", err)
	}
}

func TestCopiedEnrollmentInputAndExplicitPublicationForNonAgents(t *testing.T) {
	s, r, defs := harness(t)
	localPin := pin
	enrollment := request(actorURN)
	enrollment.Definition = &localPin
	defs.hook = func() { localPin = nextPin; defs.hook = nil }
	if err := s.EnrollActor(t.Context(), owner, enrollment); err != nil {
		t.Fatal(err)
	}
	got, err := r.Agent(t.Context(), actorURN)
	if err != nil || got.Value.Definition != pin {
		t.Fatal("caller mutated enrollment during verification", got, err)
	}
	human := mesh.URN("msg://user/example/participant")
	if err := s.EnrollActor(t.Context(), owner, Enrollment{Actor: mesh.Actor{URN: human, Kind: mesh.ActorUser}, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	otherOwner := mesh.URN("msg://user/example/another-owner")
	otherURN := mesh.URN("msg://agent/example/other-owner-agent")
	other := request(otherURN)
	other.Owner = otherOwner
	if err := s.EnrollActor(t.Context(), otherOwner, other); err != nil {
		t.Fatal(err)
	}
	s.advertise = func(_ context.Context, a Authorization, _ definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: a.Target == human}, nil
	}
	page, err := s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	foundHuman := false
	for _, record := range page.Records {
		if record.URN == otherURN || record.URN == actorURN {
			t.Fatal("wrong scope or implicit publication", record)
		}
		if record.URN == human {
			foundHuman = true
			if record.Kind != mesh.ActorUser || record.Definition != nil {
				t.Fatal("non-agent gained a definition", record)
			}
		}
	}
	if !foundHuman {
		t.Fatal("explicitly published human missing")
	}
}
