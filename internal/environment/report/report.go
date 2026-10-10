package report

import (
	"github.com/hollis-labs/tether/internal/environment"
)

// Report is the authenticated payload containing detailed environment resources
// and capabilities.
type Report struct {
	Providers        map[string]ProviderState `json:"providers"`
	BubblewrapUsable *bool                    `json:"bubblewrap_usable"` // null when not checked
	Hosting          HostingState             `json:"hosting"`
	Filesystem       FilesystemState          `json:"filesystem"`
	Resources        ResourceState            `json:"resources"`
	RoleProfile      RoleProfile              `json:"role_profile"`
}

type ProviderState struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	LoggedIn  string `json:"logged_in,omitempty"` // "true", "false", "unknown"
	Sandbox   string `json:"sandbox,omitempty"`   // configured strategy, or "unavailable" / "unknown"
}

type HostingState struct {
	SystemdUserSession string `json:"systemd_user_session"` // "true", "false", "unknown"
	LingerEnabled      string `json:"linger_enabled"`       // "true", "false", "unknown"
	LaunchHostShim     bool   `json:"launch_host_shim"`
}

type FilesystemState struct {
	ReflinkSupported string `json:"reflink_supported"` // "true", "false", "unknown"
}

type ResourceState struct {
	Status          string  `json:"status"` // "ok", "partial", "unknown"; null measurements are unknown
	CPUCount        int     `json:"cpu_count"`
	CPULoad         string  `json:"cpu_load,omitempty"`
	MemoryAvailable *uint64 `json:"memory_available"`
	StateDiskFree   *uint64 `json:"state_disk_free"`
	WorkDiskFree    *uint64 `json:"work_disk_free"`
}

type RoleProfile struct {
	Role    string   `json:"role"`
	Enabled []string `json:"enabled_modules"`
}

// CapabilityGroups describes the installed report contract, not a host's
// results. The cached public descriptor must not disclose provider inventory,
// login state, hosting selections, filesystem features or resource samples.
// Obtaining those details requires the report's verified read authority.
func CapabilityGroups() map[string]map[string]any {
	return map[string]map[string]any{
		"capability_report": {
			"version": 1, "providers": true, "sandbox": true,
			"hosting": true, "filesystem": true, "resources": true, "role_profile": true,
		},
	}
}

// FromProfile converts an environment.Profile into a RoleProfile.
func FromProfile(p *environment.Profile) RoleProfile {
	if p == nil {
		return RoleProfile{Role: "unknown", Enabled: nil}
	}
	var enabled []string
	for _, name := range environment.ModuleNames() {
		if p.Enabled(name) {
			enabled = append(enabled, name)
		}
	}
	return RoleProfile{
		Role:    p.Role,
		Enabled: enabled,
	}
}
