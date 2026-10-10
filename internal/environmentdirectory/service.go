package environmentdirectory

import (
	"context"
	"errors"
	"fmt"
	"time"

	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
)

// Storage owns transactions and enforces immutable identities and home uniqueness.
type Storage interface {
	RegisterEnvironment(context.Context, Record) (Record, error)
	GetEnvironment(context.Context, string) (Record, error)
	EnvironmentByAuthority(context.Context, string) (Record, error)
	ListEnvironments(context.Context) ([]Record, error)
	RenameEnvironment(context.Context, string, string) (Record, error)
	RetireEnvironment(context.Context, string) (Record, bool, error)
	CompleteEnvironmentRevocation(context.Context, string) (Record, error)
	ObserveEnvironment(context.Context, string, string, *tether.EnvironmentDescriptor, *time.Time) (Record, error)
}

// Revoker is an explicitly authorized worker authority port, not a token mint
// or permission inference. Nil means unsupported; retirement remains pending.
type Revoker func(context.Context, Record) error

type Options struct {
	Client tether.EnvironmentOptions
	Revoke Revoker
	Now    func() time.Time
	// ValidateBinding is a pure static-peer composition check, before I/O and commit.
	ValidateBinding func(Registration) error
}

type Service struct {
	store Storage
	opts  Options
}

func New(store Storage, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{store: store, opts: opts}
}

func (s *Service) List(ctx context.Context) ([]Record, error) { return s.store.ListEnvironments(ctx) }
func (s *Service) Get(ctx context.Context, id string) (Record, error) {
	return s.store.GetEnvironment(ctx, id)
}
func (s *Service) ByAuthority(ctx context.Context, a string) (Record, error) {
	return s.store.EnvironmentByAuthority(ctx, a)
}

// Register consumes an operator-authorized expected identity. The descriptor is
// the peer's claim; it cannot select or replace the caller's UUID or authority.
func (s *Service) Register(ctx context.Context, in Registration) (Record, error) {
	in, err := Normalize(in)
	if err != nil {
		return Record{}, err
	}
	if old, e := s.store.GetEnvironment(ctx, in.EnvironmentID); e == nil {
		if old.State == "retired" {
			return Record{}, ErrRetired
		}
		if !SameBinding(old.Registration, in) {
			return Record{}, ErrConflict
		}
	} else if !errors.Is(e, ErrNotFound) {
		return Record{}, e
	}
	if old, e := s.store.EnvironmentByAuthority(ctx, in.Authority); e == nil && old.EnvironmentID != in.EnvironmentID {
		return Record{}, ErrConflict
	} else if e != nil && !errors.Is(e, ErrNotFound) {
		return Record{}, e
	}
	if s.opts.ValidateBinding != nil {
		if err = s.opts.ValidateBinding(in); err != nil {
			return Record{}, err
		}
	}
	client, err := tether.NewEnvironmentClient(in.EnvironmentTarget, s.opts.Client)
	if err != nil {
		return Record{}, fmt.Errorf("%w: unsupported target", ErrInvalid)
	}
	conn, err := client.Connect(ctx)
	if err != nil {
		return Record{}, err
	}
	now := s.opts.Now().UTC()
	record := Record{Registration: in, Capabilities: conn.Descriptor.Capabilities, Protocol: conn.Descriptor.Protocol, ServerVersion: conn.Descriptor.ServerVersion, State: "reachable", LastSeen: &now}
	return s.store.RegisterEnvironment(ctx, record)
}

func (s *Service) Rename(ctx context.Context, id, label string) (Record, error) {
	if len(label) > 256 {
		return Record{}, ErrInvalid
	}
	return s.store.RenameEnvironment(ctx, id, label)
}

// Retire commits the routing tombstone before attempting a one-shot revoke.
// A duplicate call or daemon restart never retries an uncertain remote mutation.
func (s *Service) Retire(ctx context.Context, id string) (Record, error) {
	record, first, err := s.store.RetireEnvironment(ctx, id)
	if err != nil || !first || s.opts.Revoke == nil {
		return record, err
	}
	if err = s.opts.Revoke(ctx, record); err != nil {
		return record, nil
	}
	return s.store.CompleteEnvironmentRevocation(ctx, id)
}

// Observe is the 0079 scheduling seam. Failed observations preserve the last
// successful timestamp and can never revive retired records or change binding.
func (s *Service) Observe(ctx context.Context, id, state string, d *tether.EnvironmentDescriptor) (Record, error) {
	switch state {
	case "enrolled", "reachable", "unreachable", "incompatible":
	default:
		return Record{}, ErrInvalid
	}
	var seen *time.Time
	if d != nil {
		if d.EnvironmentID != id {
			return Record{}, &tether.EnvironmentIdentityError{}
		}
		if d.Protocol != tether.EnvironmentProtocol {
			return Record{}, &tether.ProtocolMismatchError{RequiredProtocol: d.Protocol}
		}
		if state != "reachable" {
			return Record{}, ErrInvalid
		}
		now := s.opts.Now().UTC()
		seen = &now
	} else if state == "reachable" {
		return Record{}, ErrInvalid
	}
	return s.store.ObserveEnvironment(ctx, id, state, d, seen)
}
