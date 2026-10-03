package definitionresolve

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

var ErrUnavailable = errors.New("enrollment or binding unavailable")
var ErrOtherSession = errors.New("agent binding belongs to another session")

// Resolver requires host authorization; a pin is not a permission. It does not
// enroll, acquire/renew leases, launch or bind permission names to modes.
type Resolver struct {
	repository  *fabricstore.Repository
	definitions *DefinitionStore
	authorize   func(context.Context, mesh.ResolveRequest) error
	now         func() time.Time
}

var _ mesh.Resolver = (*Resolver)(nil)

func NewResolver(repository *fabricstore.Repository, definitions *DefinitionStore, authorize func(context.Context, mesh.ResolveRequest) error, now func() time.Time) (*Resolver, error) {
	if repository == nil || definitions == nil || authorize == nil || now == nil {
		return nil, ErrConfiguration
	}
	return &Resolver{repository: repository, definitions: definitions, authorize: authorize, now: now}, nil
}
func (r *Resolver) Resolve(ctx context.Context, request mesh.ResolveRequest) (mesh.ResolveResult, error) {
	var result mesh.ResolveResult
	if err := (mesh.Actor{URN: request.AgentURN, Kind: mesh.ActorAgent}).Validate(); err != nil {
		return result, err
	}
	if request.SessionURN != "" {
		if err := request.SessionURN.Validate(); err != nil {
			return result, err
		}
	}
	if err := r.authorize(ctx, request); err != nil {
		return result, err
	}
	snapshot, err := r.repository.Snapshot(ctx, request.AgentURN, request.SessionURN)
	if err != nil {
		return result, err
	}
	if snapshot.Agent.Value.Lifecycle != mesh.EnrollmentActive {
		return result, ErrUnavailable
	}
	if _, err := r.definitions.Load(ctx, snapshot.Agent.Value.Definition); err != nil {
		return result, err
	}
	active := snapshot.Head != nil && snapshot.Head.Value.Lease != nil && snapshot.Head.Value.Lease.ExpiresAt.After(r.now())
	if active && request.SessionURN != "" && snapshot.Head.Value.Lease.SessionURN != request.SessionURN {
		return result, ErrOtherSession
	}
	if request.SessionURN != "" || active {
		if snapshot.Session == nil {
			return result, ErrUnavailable
		}
		if _, err := r.definitions.Load(ctx, snapshot.Session.Value.Definition); err != nil {
			return result, err
		}
	}
	if active {
		lease := snapshot.Head.Value.Lease
		if snapshot.Instance == nil || snapshot.Session == nil || lease.FencingToken != snapshot.Head.Value.HighWater || lease.FencingToken == 0 || snapshot.Instance.Value.BindingFence != lease.FencingToken || snapshot.Instance.Value.SessionURN != lease.SessionURN || snapshot.Instance.Value.AgentURN != request.AgentURN || snapshot.Instance.Value.Definition != snapshot.Session.Value.Definition {
			return result, ErrUnavailable
		}
	}
	// I/O cannot hold the writer. Reject a mixture of record versions instead.
	current, err := r.repository.Snapshot(ctx, request.AgentURN, request.SessionURN)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(snapshot, current) {
		return result, fabricstore.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Agent = snapshot.Agent.Value
	if request.SessionURN != "" {
		session := snapshot.Session.Value
		result.Session = &session
	}
	if active && snapshot.Head.Value.Lease.ExpiresAt.After(r.now()) {
		session := snapshot.Session.Value
		instance := snapshot.Instance.Value
		binding := *snapshot.Head.Value.Lease
		result.Session = &session
		result.Instance = &instance
		result.Binding = &binding
	}
	return result, nil
}
