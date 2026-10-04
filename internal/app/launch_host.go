package app

import (
	"os"

	"github.com/hollis-labs/tether/internal/config"
)

// LaunchHost selects where a supported provider process runs.
type LaunchHost string

const (
	HostDirect    LaunchHost = "direct"
	HostShim      LaunchHost = "shim"
	EnvLaunchHost            = "TETHER_LAUNCH_HOST"
)

// resolveLaunchHost reads the override on each call. An unrecognized override
// selects direct execution, even when the catalog requests shim hosting.
func resolveLaunchHost(cat *config.Catalog) LaunchHost {
	value := os.Getenv(EnvLaunchHost)
	if value == "" && cat != nil {
		value = cat.Global.Catalog.Defaults.LaunchHost
	}
	if value == string(HostShim) {
		return HostShim
	}
	return HostDirect
}

// LaunchHost reports the current hosting selection without creating a host.
func (s *Service) LaunchHost() LaunchHost {
	return resolveLaunchHost(s.Catalog)
}
