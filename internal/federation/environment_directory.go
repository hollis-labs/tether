package federation

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
)

// AuthorityResolver returns owned=true even for retired or unavailable directory
// authorities. Those records must never fall through to static peers or local DB.
type AuthorityResolver func(context.Context, string) (messaging.Store, bool, error)

type EnvironmentLookup interface {
	ByAuthority(context.Context, string) (directory.Record, error)
	List(context.Context) ([]directory.Record, error)
}

// ValidateDirectoryBinding refuses static configuration drift without rewriting
// config-only peer entries. Directory-backed routing always proves the UUID.
func ValidateDirectoryBinding(in directory.Registration, cfg Config) error {
	if len(in.Routes) == 0 || (cfg.LocalAuthority != "" && in.Authority == cfg.LocalAuthority) {
		return directory.ErrConflict
	}
	if !cfg.Enabled {
		return nil
	}
	for _, peer := range cfg.Peers {
		if peer.Authority == in.Authority && (strings.TrimRight(peer.BaseURL, "/") != in.Routes[0].BaseURL || peer.CredentialRef != in.CredentialReference) {
			return directory.ErrConflict
		}
	}
	return nil
}

// DirectoryAuthorityResolver queries the same committed directory used by CRUD.
// No mutable cached route projection exists to publish or drift after a commit.
func DirectoryAuthorityResolver(ctx context.Context, lookup EnvironmentLookup, cfg Config, opts tether.EnvironmentOptions) (AuthorityResolver, error) {
	records, err := lookup.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if err = ValidateDirectoryBinding(record.Registration, cfg); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, authority string) (messaging.Store, bool, error) {
		record, err := lookup.ByAuthority(ctx, authority)
		if errors.Is(err, directory.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, true, err
		}
		if record.State == "retired" || record.State == "incompatible" {
			return nil, true, ErrNoRoute
		}
		remote, err := tether.NewEnvironmentClient(record.EnvironmentTarget, opts)
		if err != nil {
			return nil, true, err
		}
		return &directoryPeerStore{remote: remote}, true, nil
	}, nil
}

// Lookup remains DB-only (including IsLocal). Each actual operation authenticates
// its expected environment before using the selected route; mutations run once.
type directoryPeerStore struct{ remote *tether.EnvironmentClient }

func (s *directoryPeerStore) store(ctx context.Context) (messaging.Store, error) {
	conn, err := s.remote.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Client.AsStore(), nil
}

func (s *directoryPeerStore) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	st, err := s.store(ctx)
	if err != nil {
		return nil, err
	}
	return st.Inbox(ctx, to, f)
}
func (s *directoryPeerStore) Subscribe(ctx context.Context, to messaging.Address, f messaging.Filter) (<-chan messaging.Envelope, error) {
	st, err := s.store(ctx)
	if err != nil {
		return nil, err
	}
	return st.Subscribe(ctx, to, f)
}
func (s *directoryPeerStore) Consume(ctx context.Context, id string, to messaging.Address) error {
	st, err := s.store(ctx)
	if err != nil {
		return err
	}
	return st.Consume(ctx, id, to)
}
func (s *directoryPeerStore) Get(ctx context.Context, id string) (messaging.Envelope, error) {
	st, err := s.store(ctx)
	if err != nil {
		return messaging.Envelope{}, err
	}
	return st.Get(ctx, id)
}
func (s *directoryPeerStore) Thread(ctx context.Context, id string, f messaging.Filter) ([]messaging.Envelope, error) {
	st, err := s.store(ctx)
	if err != nil {
		return nil, err
	}
	return st.Thread(ctx, id, f)
}
func (s *directoryPeerStore) Cancel(ctx context.Context, id string) error {
	st, err := s.store(ctx)
	if err != nil {
		return err
	}
	return st.Cancel(ctx, id)
}

func (s *directoryPeerStore) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	metadata := make(map[string]string, len(env.Metadata)+1)
	for k, v := range env.Metadata {
		if k != wakeintent.OutcomeKey {
			metadata[k] = v
		}
	}
	if metadata[wakeintent.MessageIDKey] == "" {
		metadata[wakeintent.MessageIDKey] = env.ID
		if metadata[wakeintent.MessageIDKey] == "" {
			metadata[wakeintent.MessageIDKey] = uuid.NewString()
		}
	}
	env.Metadata = metadata
	st, err := s.store(ctx)
	if err != nil {
		return messaging.Envelope{}, err
	}
	return st.Send(ctx, env)
}
