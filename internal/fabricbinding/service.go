// Package fabricbinding provides inert atomic admission and fenced writer ports.
// It is not wired to a daemon, Manager, registry endpoint or process launcher.
package fabricbinding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

var (
	ErrDenied = errors.New("binding authorization denied")
	ErrBound  = errors.New("agent already bound")
	ErrStale  = errors.New("binding fence or holder is stale")
)

type Action string

const (
	Acquire   Action = "acquire"
	Renew     Action = "renew"
	Release   Action = "release"
	Lifecycle Action = "lifecycle"
	Report    Action = "report"
)

// Authorization contains data, not grants. The host authenticates Caller and
// checks owner delegation, requested references and release evidence. Every
// non-context refusal is opaque ErrDenied, including host outages.
type Authorization struct {
	Caller, Owner, AgentURN, SessionURN                                  mesh.URN
	InstanceID                                                           string
	Definition                                                           mesh.DefinitionRef
	Action                                                               Action
	Fence                                                                uint64
	NodeRef, RuntimeRef, LaunchRecordRef, ContextRef, StoreRef, ProofRef string
}
type Authorizer func(context.Context, Authorization) error
type Definitions interface {
	Load(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error)
}

// Validator is a rejection-capable host report policy, not a publisher. It must
// be pure, bounded and non-reentrant: no I/O, side effects or Repository calls.
// Registry and outbox writes occur only after it accepts a current fence.
type Validator func(context.Context, mesh.URN, Observation) error
type Service struct {
	repo        *fabricstore.Repository
	definitions Definitions
	authorize   Authorizer
	validate    Validator
	now         func() time.Time
	maxTTL      time.Duration
}

func New(repo *fabricstore.Repository, defs Definitions, auth Authorizer, validate Validator, now func() time.Time, maxTTL time.Duration) (*Service, error) {
	if repo == nil || defs == nil || auth == nil || validate == nil || now == nil || maxTTL <= 0 || maxTTL > time.Hour {
		return nil, definitionresolve.ErrConfiguration
	}
	return &Service{repo, defs, auth, validate, now, maxTTL}, nil
}

const maxTextBytes = 4096

func textOK(s string) bool            { return len(s) > 0 && len(s) <= maxTextBytes && utf8.ValidString(s) }
func urnOK(u mesh.URN) bool           { return textOK(string(u)) && u.Validate() == nil }
func pinOK(p mesh.DefinitionRef) bool { return textOK(p.ID) && textOK(p.Revision) && textOK(p.Digest) }
func opaque(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("%w", ErrDenied)
}

const validationTimeout = 100 * time.Millisecond

// Validators must respect this short context and must not use the repository.
func (s *Service) validateObservation(ctx context.Context, caller mesh.URN, o Observation) error {
	bounded, cancel := context.WithTimeout(ctx, validationTimeout)
	defer cancel()
	return opaque(bounded, s.validate(bounded, caller, o))
}

func (s *Service) clock() (time.Time, error) {
	at := s.now().UTC()
	if at.IsZero() {
		return at, fabricstore.ErrInvalid
	}
	return at, nil
}
func (s *Service) ttl(ttl time.Duration) bool { return ttl > 0 && ttl <= s.maxTTL }
func digest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("fabric-binding-v1\n"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func (s *Service) agent(ctx context.Context, caller, urn mesh.URN) (fabricstore.Record[mesh.Agent], error) {
	var empty fabricstore.Record[mesh.Agent]
	if !urnOK(caller) || !urnOK(urn) {
		return empty, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	a, err := s.repo.Agent(ctx, urn)
	if errors.Is(err, fabricstore.ErrNotFound) {
		return empty, ErrDenied
	}
	if err != nil {
		return empty, err
	}
	if !urnOK(a.Value.Owner) || !pinOK(a.Value.Definition) {
		return empty, fabricstore.ErrInvalid
	}
	return a, nil
}
func (s *Service) allow(ctx context.Context, a Authorization) error {
	return opaque(ctx, s.authorize(ctx, a))
}
func (s *Service) verify(ctx context.Context, p mesh.DefinitionRef) error {
	if !pinOK(p) {
		return fabricstore.ErrInvalid
	}
	v, err := s.definitions.Load(ctx, p)
	if err != nil {
		return err
	}
	if v.Pin != p || v.Definition == nil {
		return definitionresolve.ErrPinMismatch
	}
	return nil
}
func checkAgent(tx *fabricstore.Tx, before fabricstore.Record[mesh.Agent], requireActive bool) error {
	a, err := tx.Agent(before.Value.URN)
	if err != nil {
		return err
	}
	if a.Version != before.Version {
		return fabricstore.ErrConflict
	}
	if requireActive && a.Value.Lifecycle != mesh.EnrollmentActive {
		return ErrDenied
	}
	return nil
}
