package config

import "github.com/hollis-labs/tether/internal/environment"

// EnvironmentConfig describes this state's identity, not a hub enrollment.
// Authority is optional until enrollment; once configured it is pinned locally
// to the durable environment UUID. Collision checks belong to the directory.
type EnvironmentConfig struct {
	Label     string `yaml:"label,omitempty"`
	Authority string `yaml:"authority,omitempty"`
}

func (c EnvironmentConfig) Validate() error { return environment.ValidateAuthority(c.Authority) }
