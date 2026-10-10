package fabricstore

import (
	"context"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
)

// DefinitionRevision stores references and digests, never catalog content or
// credentials. Digest interpretation/verification belongs to the resolver.
type DefinitionRevision struct {
	Definition mesh.DefinitionRef `json:"definition"`
	SourceRef  string             `json:"source_ref"`
}
type DefinitionArtifact struct {
	Definition      mesh.DefinitionRef `json:"definition"`
	Digest          string             `json:"digest"`
	SourceRef       string             `json:"source_ref"`
	VerificationRef string             `json:"verification_ref"`
}
type Actor struct {
	mesh.Actor
	Authority EnrollmentAuthority      `json:"authority"`
	Owner     mesh.URN                 `json:"owner"`
	Lifecycle mesh.EnrollmentLifecycle `json:"lifecycle"`
}

func validPin(pin mesh.DefinitionRef) bool {
	return pin.ID != "" && pin.Revision != "" && pin.Digest != ""
}
func validLifecycle(l mesh.EnrollmentLifecycle) bool {
	return l == mesh.EnrollmentActive || l == mesh.EnrollmentRetired
}
func (tx *Tx) requirePin(pin mesh.DefinitionRef) error {
	if !validPin(pin) {
		return invalid("definition pin is incomplete")
	}
	revision, err := read[DefinitionRevision](tx.ctx, tx.conn, definitions, tuple(pin.ID, pin.Revision))
	if err != nil {
		return err
	}
	if revision.Value.Definition != pin {
		return invalid("definition pin does not match revision")
	}
	return nil
}
func (tx *Tx) AddDefinition(value DefinitionRevision) error {
	if !validPin(value.Definition) || value.SourceRef == "" {
		return invalid("definition reference is incomplete")
	}
	return put(tx, definitions, tuple(value.Definition.ID, value.Definition.Revision), value, 0)
}
func (r *Repository) Definition(ctx context.Context, id, revision string) (Record[DefinitionRevision], error) {
	return read[DefinitionRevision](ctx, r.db, definitions, tuple(id, revision))
}
func (tx *Tx) AddArtifact(value DefinitionArtifact) error {
	if value.Digest == "" || value.SourceRef == "" || value.VerificationRef == "" {
		return invalid("artifact provenance is incomplete")
	}
	if err := tx.requirePin(value.Definition); err != nil {
		return err
	}
	return put(tx, artifacts, tuple(value.Definition.ID, value.Definition.Revision, value.Digest), value, 0)
}
func (r *Repository) Artifact(ctx context.Context, pin mesh.DefinitionRef, digest string) (Record[DefinitionArtifact], error) {
	return read[DefinitionArtifact](ctx, r.db, artifacts, tuple(pin.ID, pin.Revision, digest))
}
func (tx *Tx) PutActor(value Actor, expected int64) error {
	if err := value.Authority.Validate(); err != nil {
		return err
	}
	value.Authority = value.Authority.Effective()
	if err := value.Validate(); err != nil {
		return invalid(err.Error())
	}
	if value.Owner.Validate() != nil || !validLifecycle(value.Lifecycle) {
		return invalid("actor owner/lifecycle is invalid")
	}
	if expected > 0 {
		old, err := read[Actor](tx.ctx, tx.conn, actors, string(value.URN))
		if err != nil {
			return err
		}
		if old.Value.Kind != value.Kind || old.Value.Owner != value.Owner {
			return invalid("actor kind and owner are immutable")
		}
		if old.Value.Lifecycle == mesh.EnrollmentRetired && value.Lifecycle != mesh.EnrollmentRetired {
			return invalid("retired actor cannot reactivate")
		}
	}
	return put(tx, actors, string(value.URN), value, expected)
}
func (r *Repository) Actor(ctx context.Context, urn mesh.URN) (Record[Actor], error) {
	return read[Actor](ctx, r.db, actors, string(urn))
}
func (tx *Tx) PutAgent(value mesh.Agent, expected int64) error {
	actor, err := read[Actor](tx.ctx, tx.conn, actors, string(value.URN))
	if err != nil {
		return err
	}
	if actor.Value.Kind != mesh.ActorAgent || actor.Value.Owner != value.Owner || actor.Value.Lifecycle != value.Lifecycle {
		return invalid("agent must match enrolled actor")
	}
	if !validLifecycle(value.Lifecycle) {
		return invalid("agent lifecycle is invalid")
	}
	if err := tx.requirePin(value.Definition); err != nil {
		return err
	}
	return put(tx, agents, string(value.URN), value, expected)
}
func (r *Repository) Agent(ctx context.Context, urn mesh.URN) (Record[mesh.Agent], error) {
	return read[mesh.Agent](ctx, r.db, agents, string(urn))
}

// Snapshot reads a consistent enrollment, session, instance and head view without
// verifying content or making claims about current lease authority.
type Snapshot struct {
	Agent    Record[mesh.Agent]
	Session  *Record[mesh.Session]
	Instance *Record[mesh.AgentInstance]
	Head     *Record[BindingHead]
}

func (r *Repository) Snapshot(ctx context.Context, agentURN, sessionURN mesh.URN) (Snapshot, error) {
	var result Snapshot
	tx, err := r.db.BeginTx(ctx, &sqlReadOnly)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	result.Agent, err = read[mesh.Agent](ctx, tx, agents, string(agentURN))
	if err != nil {
		return result, err
	}
	if sessionURN != "" {
		session, err := read[mesh.Session](ctx, tx, sessions, string(sessionURN))
		if err != nil {
			return result, err
		}
		if session.Value.AgentURN != agentURN {
			return result, invalid("session belongs to another agent")
		}
		result.Session = &session
	}
	head, err := read[BindingHead](ctx, tx, heads, string(agentURN))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return result, err
	}
	if err == nil {
		result.Head = &head
		if head.Value.Lease != nil {
			instance, err := read[mesh.AgentInstance](ctx, tx, instances, head.Value.Lease.InstanceID)
			if err != nil {
				return result, err
			}
			result.Instance = &instance
			if sessionURN == "" {
				session, err := read[mesh.Session](ctx, tx, sessions, string(head.Value.Lease.SessionURN))
				if err != nil {
					return result, err
				}
				if session.Value.AgentURN != agentURN {
					return result, invalid("binding session belongs to another agent")
				}
				result.Session = &session
			}
		}
	}
	return result, tx.Commit()
}

// Definition and Artifact read within the reserved writer transaction.
func (tx *Tx) Definition(id, revision string) (Record[DefinitionRevision], error) {
	return read[DefinitionRevision](tx.ctx, tx.conn, definitions, tuple(id, revision))
}
func (tx *Tx) Artifact(pin mesh.DefinitionRef, digest string) (Record[DefinitionArtifact], error) {
	return read[DefinitionArtifact](tx.ctx, tx.conn, artifacts, tuple(pin.ID, pin.Revision, digest))
}
