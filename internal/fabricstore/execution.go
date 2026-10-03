package fabricstore

import (
	"context"
	"math"
	"time"

	"github.com/hollis-labs/substrate/mesh"
)

func (tx *Tx) PutSession(value mesh.Session, expected int64) error {
	if value.URN.Validate() != nil || !value.State.Valid() || value.ContextRef == "" || value.StoreRef == "" || value.CreatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
		return invalid("session fields are invalid")
	}
	agent, err := read[mesh.Agent](tx.ctx, tx.conn, agents, string(value.AgentURN))
	if err != nil {
		return err
	}
	if expected == 0 && (agent.Value.Lifecycle != mesh.EnrollmentActive || agent.Value.Definition != value.Definition) {
		return invalid("new session must pin active agent definition")
	}
	if err := tx.requirePin(value.Definition); err != nil {
		return err
	}
	if expected > 0 {
		old, err := read[mesh.Session](tx.ctx, tx.conn, sessions, string(value.URN))
		if err != nil {
			return err
		}
		if old.Value.AgentURN != value.AgentURN || old.Value.Definition != value.Definition || old.Value.ContextRef != value.ContextRef || old.Value.StoreRef != value.StoreRef || !old.Value.CreatedAt.Equal(value.CreatedAt) || value.UpdatedAt.Before(old.Value.UpdatedAt) {
			return invalid("session identity/pin/context are immutable and time cannot regress")
		}
	}
	return put(tx, sessions, string(value.URN), value, expected)
}
func (r *Repository) Session(ctx context.Context, urn mesh.URN) (Record[mesh.Session], error) {
	return read[mesh.Session](ctx, r.db, sessions, string(urn))
}
func (tx *Tx) PutInstance(value mesh.AgentInstance, expected int64) error {
	if value.ID == "" || value.NodeRef == "" || value.RuntimeRef == "" || value.LaunchRecordRef == "" || value.BindingFence > math.MaxInt64 || value.CreatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
		return invalid("instance fields are invalid")
	}
	session, err := read[mesh.Session](tx.ctx, tx.conn, sessions, string(value.SessionURN))
	if err != nil {
		return err
	}
	if session.Value.AgentURN != value.AgentURN || session.Value.Definition != value.Definition {
		return invalid("instance must match pinned session")
	}
	if _, err := mesh.ProjectState(value.Status, value.Detail, session.Value.State); err != nil {
		return invalid(err.Error())
	}
	if expected > 0 {
		old, err := read[mesh.AgentInstance](tx.ctx, tx.conn, instances, value.ID)
		if err != nil {
			return err
		}
		previous := old.Value
		if previous.AgentURN != value.AgentURN || previous.SessionURN != value.SessionURN || previous.Definition != value.Definition || previous.NodeRef != value.NodeRef || previous.RuntimeRef != value.RuntimeRef || previous.LaunchRecordRef != value.LaunchRecordRef || (previous.BindingFence != 0 && previous.BindingFence != value.BindingFence) || !previous.CreatedAt.Equal(value.CreatedAt) || value.UpdatedAt.Before(previous.UpdatedAt) {
			return invalid("instance identity/pin/fence are immutable and time cannot regress")
		}
	}
	return put(tx, instances, value.ID, value, expected)
}
func (r *Repository) Instance(ctx context.Context, id string) (Record[mesh.AgentInstance], error) {
	return read[mesh.AgentInstance](ctx, r.db, instances, id)
}

// BindingHead retains the high-water mark when no lease exists. It is storage,
// not an acquire/renew/release API; admission must supply authenticated policy,
// expiry checks and fencing of every authoritative write before activation.
type BindingHead struct {
	AgentURN  mesh.URN           `json:"agent_urn"`
	HighWater uint64             `json:"high_water"`
	Lease     *mesh.BindingLease `json:"lease,omitempty"`
}

func (tx *Tx) PutBindingHead(value BindingHead, expected int64) error {
	if value.HighWater > math.MaxInt64 {
		return invalid("binding fence exceeds SQLite range")
	}
	agent, err := read[mesh.Agent](tx.ctx, tx.conn, agents, string(value.AgentURN))
	if err != nil {
		return err
	}
	var old BindingHead
	if expected > 0 {
		record, err := read[BindingHead](tx.ctx, tx.conn, heads, string(value.AgentURN))
		if err != nil {
			return err
		}
		if record.Version != expected {
			return ErrConflict
		}
		old = record.Value
	}
	if value.HighWater < old.HighWater {
		return invalid("binding fence cannot regress")
	}
	if value.Lease != nil {
		lease := value.Lease
		if agent.Value.Lifecycle != mesh.EnrollmentActive || lease.AgentURN != value.AgentURN || lease.FencingToken == 0 || lease.FencingToken != value.HighWater || lease.Holder.Validate() != nil || lease.ExpiresAt.IsZero() {
			return invalid("binding fields are invalid")
		}
		instance, err := read[mesh.AgentInstance](tx.ctx, tx.conn, instances, lease.InstanceID)
		if err != nil {
			return err
		}
		if instance.Value.AgentURN != value.AgentURN || instance.Value.SessionURN != lease.SessionURN || instance.Value.BindingFence != lease.FencingToken {
			return invalid("binding does not match instance fence/session")
		}
		same := old.Lease != nil && old.Lease.InstanceID == lease.InstanceID && old.Lease.SessionURN == lease.SessionURN && old.Lease.Holder == lease.Holder && old.Lease.FencingToken == lease.FencingToken
		if !same && value.HighWater <= old.HighWater {
			return invalid("new holder requires greater fence")
		}
		if same && lease.ExpiresAt.Before(old.Lease.ExpiresAt) {
			return invalid("renewal cannot shorten expiry")
		}
	} else if value.HighWater != old.HighWater {
		return invalid("unheld head cannot advance fence")
	}
	return put(tx, heads, string(value.AgentURN), value, expected)
}
func (r *Repository) BindingHead(ctx context.Context, urn mesh.URN) (Record[BindingHead], error) {
	return read[BindingHead](ctx, r.db, heads, string(urn))
}

type BindingEvent struct {
	ID    string            `json:"id"`
	Lease mesh.BindingLease `json:"lease"`
	Kind  string            `json:"kind"`
	At    time.Time         `json:"at"`
}

func (tx *Tx) AddBindingEvent(value BindingEvent) error {
	if value.ID == "" || value.At.IsZero() || (value.Kind != "acquired" && value.Kind != "renewed" && value.Kind != "released" && value.Kind != "expired") {
		return invalid("binding event is invalid")
	}
	head, err := read[BindingHead](tx.ctx, tx.conn, heads, string(value.Lease.AgentURN))
	if err != nil {
		return err
	}
	if value.Lease.FencingToken == 0 || value.Lease.FencingToken > head.Value.HighWater {
		return invalid("binding event fence is invalid")
	}
	instance, err := read[mesh.AgentInstance](tx.ctx, tx.conn, instances, value.Lease.InstanceID)
	if err != nil {
		return err
	}
	if instance.Value.AgentURN != value.Lease.AgentURN || instance.Value.SessionURN != value.Lease.SessionURN || instance.Value.BindingFence != value.Lease.FencingToken {
		return invalid("binding history does not match instance")
	}
	return put(tx, history, value.ID, value, 0)
}
func (r *Repository) BindingEvent(ctx context.Context, id string) (Record[BindingEvent], error) {
	return read[BindingEvent](ctx, r.db, history, id)
}
