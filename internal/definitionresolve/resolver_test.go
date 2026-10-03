package definitionresolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

const agentURN mesh.URN = "msg://agent/example/durable"
const ownerURN mesh.URN = "msg://user/example/operator"
const sessionURN mesh.URN = "urn:session:example"

var clockTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func resolverHarness(t *testing.T) (*Resolver, *DefinitionStore, *fabricstore.Repository, *fakeContent, mesh.DefinitionRef) {
	t.Helper()
	definitions, repo, provider := definitionHarness(t, Policy{})
	indexed, err := definitions.Index(t.Context(), "catalog:definition")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if err := tx.PutActor(fabricstore.Actor{Actor: mesh.Actor{URN: agentURN, Kind: mesh.ActorAgent}, Owner: ownerURN, Lifecycle: mesh.EnrollmentActive}, 0); err != nil {
			return err
		}
		return tx.PutAgent(mesh.Agent{URN: agentURN, Owner: ownerURN, Definition: indexed.Pin, Lifecycle: mesh.EnrollmentActive}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(repo, definitions, func(context.Context, mesh.ResolveRequest) error { return nil }, func() time.Time { return clockTime })
	if err != nil {
		t.Fatal(err)
	}
	return resolver, definitions, repo, provider, indexed.Pin
}
func bindInstance(t *testing.T, repo *fabricstore.Repository, pin mesh.DefinitionRef) {
	t.Helper()
	if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if err := tx.PutSession(mesh.Session{URN: sessionURN, AgentURN: agentURN, Definition: pin, ContextRef: "urn:context:example", StoreRef: "urn:store:example", State: mesh.SessionPaused, CreatedAt: clockTime, UpdatedAt: clockTime}, 0); err != nil {
			return err
		}
		if err := tx.PutInstance(mesh.AgentInstance{ID: "instance", AgentURN: agentURN, Definition: pin, NodeRef: "urn:node:example", RuntimeRef: "urn:runtime:example", SessionURN: sessionURN, BindingFence: 1, LaunchRecordRef: "urn:launch:example", Status: mesh.InstanceWaiting, Detail: mesh.InstanceDetail{Waiting: mesh.WaitingApproval}, CreatedAt: clockTime, UpdatedAt: clockTime}, 0); err != nil {
			return err
		}
		return tx.PutBindingHead(fabricstore.BindingHead{AgentURN: agentURN, HighWater: 1, Lease: &mesh.BindingLease{AgentURN: agentURN, SessionURN: sessionURN, InstanceID: "instance", Holder: ownerURN, ExpiresAt: clockTime.Add(time.Minute), FencingToken: 1}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
}
func TestResolverIdleBoundExpiredAndPinnedHistoricalSession(t *testing.T) {
	resolver, definitions, repo, provider, pin := resolverHarness(t)
	request := mesh.ResolveRequest{AgentURN: agentURN}
	idle, err := resolver.Resolve(t.Context(), request)
	if err != nil || idle.Agent.URN != agentURN || idle.Session != nil || idle.Instance != nil || idle.Binding != nil {
		t.Fatal(idle, err)
	}
	bindInstance(t, repo, pin)
	bound, err := resolver.Resolve(t.Context(), request)
	if err != nil || bound.Session == nil || bound.Session.State != mesh.SessionPaused || bound.Instance.Detail.Waiting != mesh.WaitingApproval || bound.Binding.FencingToken != 1 {
		t.Fatal(bound, err)
	}
	// The enrolled agent moves forward; the existing conversation stays pinned.
	next := minimalDefinition()
	next.Revision = "r2"
	next.Body = "Next revision."
	provider.documents["catalog:next"] = authored(t, next)
	indexed, err := definitions.Index(t.Context(), "catalog:next")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := repo.Agent(t.Context(), agentURN)
	if err != nil {
		t.Fatal(err)
	}
	agent.Value.Definition = indexed.Pin
	if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error { return tx.PutAgent(agent.Value, agent.Version) }); err != nil {
		t.Fatal(err)
	}
	pinned, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN, SessionURN: sessionURN})
	if err != nil || pinned.Agent.Definition != indexed.Pin || pinned.Session.Definition != pin || pinned.Instance.Definition != pin {
		t.Fatal(pinned, err)
	}
	resolver.now = func() time.Time { return clockTime.Add(2 * time.Minute) }
	expired, err := resolver.Resolve(t.Context(), request)
	if err != nil || expired.Binding != nil || expired.Instance != nil || expired.Session != nil {
		t.Fatal("expired lease reported as current", expired, err)
	}
	historical, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN, SessionURN: sessionURN})
	if err != nil || historical.Session.Definition != pin || historical.Binding != nil || historical.Instance != nil {
		t.Fatal(historical, err)
	}
	// Expiry is reporting, not release: the persisted fence is untouched.
	head, err := repo.BindingHead(t.Context(), agentURN)
	if err != nil || head.Value.HighWater != 1 || head.Value.Lease == nil {
		t.Fatal(head, err)
	}
	provider.documents["catalog:definition"] = []byte("historical source unavailable")
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN, SessionURN: sessionURN}); err == nil {
		t.Fatal("historical session pin was not verified")
	}
	if _, err := resolver.Resolve(t.Context(), request); err != nil {
		t.Fatal("expired historical source prevented idle current enrollment resolution", err)
	}
}
func TestResolverRefusesAuthorizationRetirementAndMixedSessions(t *testing.T) {
	resolver, _, repo, provider, pin := resolverHarness(t)
	denied := errors.New("not authorized")
	resolver.authorize = func(context.Context, mesh.ResolveRequest) error { return denied }
	reads := provider.reads
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if provider.reads != reads {
		t.Fatal("unauthorized request read authored content")
	}
	resolver.authorize = func(context.Context, mesh.ResolveRequest) error { return nil }
	bindInstance(t, repo, pin)
	other := mesh.URN("urn:session:other")
	if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error {
		return tx.PutSession(mesh.Session{URN: other, AgentURN: agentURN, Definition: pin, ContextRef: "urn:context:other", StoreRef: "urn:store:other", State: mesh.SessionEnded, CreatedAt: clockTime, UpdatedAt: clockTime}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN, SessionURN: other}); !errors.Is(err, ErrOtherSession) {
		t.Fatal("mixed current binding and historical session", err)
	}
	actor, err := repo.Actor(t.Context(), agentURN)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := repo.Agent(t.Context(), agentURN)
	if err != nil {
		t.Fatal(err)
	}
	actor.Value.Lifecycle = mesh.EnrollmentRetired
	agent.Value.Lifecycle = mesh.EnrollmentRetired
	if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error {
		if err := tx.PutActor(actor.Value, actor.Version); err != nil {
			return err
		}
		return tx.PutAgent(agent.Value, agent.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("retired agent resolved", err)
	}
}
func TestResolverRechecksVersionsAfterContentIO(t *testing.T) {
	resolver, _, repo, provider, _ := resolverHarness(t)
	provider.onRead = func() {
		provider.onRead = nil
		agent, err := repo.Agent(t.Context(), agentURN)
		if err != nil {
			t.Fatal(err)
		}
		// Even an otherwise identical optimistic write invalidates the snapshot.
		if err := repo.Write(t.Context(), func(tx *fabricstore.Tx) error { return tx.PutAgent(agent.Value, agent.Version) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN}); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("stale enrollment snapshot returned", err)
	}
	result, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN})
	if err != nil || result.Agent.URN != agentURN {
		t.Fatal(result, err)
	}
}
func TestResolverRechecksExpiryAndDependencyAvailability(t *testing.T) {
	resolver, _, repo, provider, pin := resolverHarness(t)
	bindInstance(t, repo, pin)
	calls := 0
	resolver.now = func() time.Time {
		calls++
		if calls == 1 {
			return clockTime
		}
		return clockTime.Add(2 * time.Minute)
	}
	result, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN})
	if err != nil || result.Binding != nil || result.Instance != nil {
		t.Fatal("lease expired during verification", result, err)
	}
	provider.documents["catalog:definition"] = []byte("invalid definition")
	if _, err := resolver.Resolve(t.Context(), mesh.ResolveRequest{AgentURN: agentURN}); err == nil {
		t.Fatal("cached definition accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := resolver.Resolve(ctx, mesh.ResolveRequest{AgentURN: agentURN}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
