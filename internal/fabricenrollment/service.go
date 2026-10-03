// Package fabricenrollment provides inert, host-authorized enrollment kernels.
// No production route, daemon composition or legacy registration calls it.
package fabricenrollment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

var (
	ErrDenied   = errors.New("enrollment authorization denied")
	ErrBound    = errors.New("enrollment binding must be explicitly released")
	ErrRetired  = errors.New("enrollment is retired")
	ErrManifest = errors.New("invalid or ambiguous import manifest")
)

type Action string

const (
	Enroll        Action = "enroll"
	Rebind        Action = "rebind"
	Retire        Action = "retire"
	ReadDirectory Action = "directory.read"
	Import        Action = "import"
)

// Authorization is evaluated by the authenticated host, including ownership
// and any explicit delegation. Owner references never grant their own authority.
type Authorization struct {
	Caller, Owner, Target mesh.URN
	Action                Action
}
type Authorizer func(context.Context, Authorization) error
type Definitions interface {
	Load(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error)
}

// Publication contains only currently allowed offered capability IDs. The host
// applies publication policy; definitions and enrollment alone publish nothing.
type Publication struct {
	Publish      bool
	Capabilities []string
}
type Advertiser func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error)

type Service struct {
	repo         *fabricstore.Repository
	definitions  Definitions
	authorize    Authorizer
	advertise    Advertiser
	now          func() time.Time
	directoryTTL time.Duration
}

func New(repo *fabricstore.Repository, definitions Definitions, authorize Authorizer, advertise Advertiser, now func() time.Time, directoryTTL time.Duration) (*Service, error) {
	if repo == nil || definitions == nil || authorize == nil || advertise == nil || now == nil || directoryTTL <= 0 || directoryTTL > time.Hour {
		return nil, definitionresolve.ErrConfiguration
	}
	return &Service{repo: repo, definitions: definitions, authorize: authorize, advertise: advertise, now: now, directoryTTL: directoryTTL}, nil
}
func (s *Service) allow(ctx context.Context, caller, owner, target mesh.URN, action Action) error {
	if caller.Validate() != nil || owner.Validate() != nil {
		return ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.authorize(ctx, Authorization{caller, owner, target, action})
}

type Enrollment struct {
	Actor      mesh.Actor
	Owner      mesh.URN
	Definition *mesh.DefinitionRef
}

func validateEnrollment(request Enrollment) error {
	if request.Actor.Validate() != nil || request.Owner.Validate() != nil {
		return fabricstore.ErrInvalid
	}
	if (request.Actor.Kind == mesh.ActorAgent) != (request.Definition != nil) {
		return fabricstore.ErrInvalid
	}
	if request.Definition != nil && (request.Definition.ID == "" || request.Definition.Revision == "" || request.Definition.Digest == "") {
		return fabricstore.ErrInvalid
	}
	return nil
}
func (s *Service) verify(ctx context.Context, request Enrollment) error {
	if request.Definition == nil {
		return nil
	}
	verified, err := s.definitions.Load(ctx, *request.Definition)
	if err != nil {
		return err
	}
	if verified.Pin != *request.Definition || verified.Definition == nil {
		return definitionresolve.ErrPinMismatch
	}
	return nil
}
func create(tx *fabricstore.Tx, request Enrollment) error {
	if err := tx.PutActor(fabricstore.Actor{Actor: request.Actor, Owner: request.Owner, Lifecycle: mesh.EnrollmentActive}, 0); err != nil {
		return err
	}
	if request.Definition != nil {
		return tx.PutAgent(mesh.Agent{URN: request.Actor.URN, Owner: request.Owner, Definition: *request.Definition, Lifecycle: mesh.EnrollmentActive}, 0)
	}
	return nil
}

// EnrollActor creates a new identity; it never merges, aliases or reactivates an
// existing record. Non-agent actors have no definition and cannot execute agents.
func (s *Service) EnrollActor(ctx context.Context, caller mesh.URN, request Enrollment) error {
	// Copy the caller-owned pin before any callback or content I/O.
	if request.Definition != nil {
		pin := *request.Definition
		request.Definition = &pin
	}
	if err := validateEnrollment(request); err != nil {
		return err
	}
	if err := s.allow(ctx, caller, request.Owner, request.Actor.URN, Enroll); err != nil {
		return err
	}
	if err := s.verify(ctx, request); err != nil {
		return err
	}
	return s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		if err := create(tx, request); err != nil {
			return err
		}
		return s.event(tx, request, Enroll, 1, mesh.EnrollmentActive)
	})
}

// RebindAgent changes only future session admission. Existing session pins are
// untouched. Even an expired stored lease must be explicitly reconciled/released.
func (s *Service) RebindAgent(ctx context.Context, caller, urn mesh.URN, pin mesh.DefinitionRef, expected int64) error {
	if expected <= 0 {
		return fabricstore.ErrInvalid
	}
	old, err := s.repo.Agent(ctx, urn)
	if err != nil {
		return err
	}
	if err := s.allow(ctx, caller, old.Value.Owner, urn, Rebind); err != nil {
		return err
	}
	if old.Version != expected {
		return fabricstore.ErrConflict
	}
	if old.Value.Lifecycle != mesh.EnrollmentActive {
		return ErrRetired
	}
	if err := s.verify(ctx, Enrollment{Actor: mesh.Actor{URN: urn, Kind: mesh.ActorAgent}, Owner: old.Value.Owner, Definition: &pin}); err != nil {
		return err
	}
	return s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		current, err := tx.Agent(urn)
		if err != nil {
			return err
		}
		if current != old {
			return fabricstore.ErrConflict
		}
		if err := unbound(tx, urn); err != nil {
			return err
		}
		current.Value.Definition = pin
		if err := tx.PutAgent(current.Value, expected); err != nil {
			return err
		}
		return s.event(tx, Enrollment{Actor: mesh.Actor{URN: urn, Kind: mesh.ActorAgent}, Owner: current.Value.Owner, Definition: &pin}, Rebind, expected+1, mesh.EnrollmentActive)
	})
}
func unbound(tx *fabricstore.Tx, urn mesh.URN) error {
	head, err := tx.BindingHead(urn)
	if errors.Is(err, fabricstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if head.Value.Lease != nil {
		return ErrBound
	}
	return nil
}

// RetireActor retains the exact identity and history. It does not stop a process
// or release a lease; retirement requires prior explicit binding release.
func (s *Service) RetireActor(ctx context.Context, caller, urn mesh.URN, expected int64) error {
	if expected <= 0 {
		return fabricstore.ErrInvalid
	}
	old, err := s.repo.Actor(ctx, urn)
	if err != nil {
		return err
	}
	if err := s.allow(ctx, caller, old.Value.Owner, urn, Retire); err != nil {
		return err
	}
	if old.Version != expected {
		return fabricstore.ErrConflict
	}
	return s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		current, err := tx.Actor(urn)
		if err != nil {
			return err
		}
		if current != old {
			return fabricstore.ErrConflict
		}
		if current.Value.Lifecycle == mesh.EnrollmentRetired {
			return nil
		}
		if current.Value.Kind == mesh.ActorAgent {
			if err := unbound(tx, urn); err != nil {
				return err
			}
			agent, err := tx.Agent(urn)
			if err != nil {
				return err
			}
			if agent.Value.Owner != current.Value.Owner {
				return fmt.Errorf("%w: owner mismatch", fabricstore.ErrInvalid)
			}
			agent.Value.Lifecycle = mesh.EnrollmentRetired
			if err := tx.PutActor(fabricstore.Actor{Actor: current.Value.Actor, Owner: current.Value.Owner, Lifecycle: mesh.EnrollmentRetired}, expected); err != nil {
				return err
			}
			if err := tx.PutAgent(agent.Value, agent.Version); err != nil {
				return err
			}
			return s.event(tx, Enrollment{Actor: current.Value.Actor, Owner: current.Value.Owner, Definition: &agent.Value.Definition}, Retire, expected+1, mesh.EnrollmentRetired)
		}
		current.Value.Lifecycle = mesh.EnrollmentRetired
		if err := tx.PutActor(current.Value, expected); err != nil {
			return err
		}
		return s.event(tx, Enrollment{Actor: current.Value.Actor, Owner: current.Value.Owner}, Retire, expected+1, mesh.EnrollmentRetired)
	})
}

func (s *Service) event(tx *fabricstore.Tx, request Enrollment, action Action, version int64, lifecycle mesh.EnrollmentLifecycle) error {
	payload, err := json.Marshal(struct {
		Actor      mesh.Actor               `json:"actor"`
		Definition *mesh.DefinitionRef      `json:"definition,omitempty"`
		Lifecycle  mesh.EnrollmentLifecycle `json:"lifecycle"`
		Version    int64                    `json:"version"`
	}{request.Actor, request.Definition, lifecycle, version})
	if err != nil {
		return err
	}
	id := digest("fabric-enrollment-event-v1", []string{string(request.Actor.URN), string(action), fmt.Sprint(version)})
	return tx.AppendEvent(fabricstore.Event{ID: id, AggregateURN: request.Actor.URN, Type: "enrollment." + string(action), Payload: payload, CreatedAt: s.now().UTC()})
}
