package config

import "github.com/hollis-labs/tether/internal/environment"

func (g Global) Profile() (*environment.Profile, error) {
	return environment.ResolveProfile(g.Role, g.Modules, g.Teams.Enabled, len(g.AI.Providers) > 0)
}
