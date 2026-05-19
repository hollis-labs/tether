package federation

import (
	"errors"
	"fmt"

	messaging "github.com/hollis-labs/go-messaging"
)

// BuildRouter composes a federation Config, the daemon's local messaging
// store, and a peer Dialer into a ready Router.
//
// When cfg.Enabled is false it returns (nil, nil): the caller keeps using
// the bare local store and nothing about messaging changes. When enabled
// it validates the config, constructs the Router around local, and
// registers every configured peer via dial. A config with peers but a nil
// dial is a wiring error and fails fast.
//
// dial is only consulted when peers are configured, so an enabled-but-
// peerless install (a declared local authority, no federation partners)
// builds successfully with a nil dial.
func BuildRouter(cfg Config, local messaging.Store, dial Dialer) (*Router, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if local == nil {
		return nil, errors.New("federation: BuildRouter requires a non-nil local store")
	}
	if len(cfg.Peers) > 0 && dial == nil {
		return nil, fmt.Errorf("federation: %d peer(s) configured but no dialer supplied", len(cfg.Peers))
	}

	var opts []RouterOption
	if cfg.Strict {
		opts = append(opts, WithStrictRouting())
	}
	r := NewRouter(local, cfg.LocalAuthority, opts...)

	for _, p := range cfg.Peers {
		store, err := dial(p)
		if err != nil {
			return nil, fmt.Errorf("federation: dial peer %q: %w", p.Authority, err)
		}
		if err := r.Register(p.Authority, store); err != nil {
			return nil, fmt.Errorf("federation: register peer %q: %w", p.Authority, err)
		}
	}
	return r, nil
}
