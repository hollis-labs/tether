package report

import (
	"github.com/hollis-labs/tether/internal/environment"
)

// Report is the authenticated payload containing detailed environment resources
// and capabilities.
type Report struct {
	Providers   map[string]ProviderState `json:"providers"`
	Hosting     HostingState             `json:"hosting"`
	Filesystem  FilesystemState          `json:"filesystem"`
	Resources   ResourceState            `json:"resources"`
	RoleProfile RoleProfile              `json:"role_profile"`
}

type ProviderState struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	LoggedIn  string `json:"logged_in,omitempty"` // "true", "false", "unknown"
	Sandbox   string `json:"sandbox,omitempty"`   // "wrapped", "protected", "not protected"
}

type HostingState struct {
	SystemdUserSession bool `json:"systemd_user_session"`
	LingerEnabled      bool `json:"linger_enabled"`
	LaunchHostShim     bool `json:"launch_host_shim"`
}

type FilesystemState struct {
	ReflinkSupported string `json:"reflink_supported"` // "true", "false", "unknown"
}

type ResourceState struct {
	CPUCount        int    `json:"cpu_count"`
	CPULoad         string `json:"cpu_load,omitempty"`
	MemoryAvailable uint64 `json:"memory_available,omitempty"`
	StateDiskFree   uint64 `json:"state_disk_free,omitempty"`
	WorkDiskFree    uint64 `json:"work_disk_free,omitempty"`
}

type RoleProfile struct {
	Role    string   `json:"role"`
	Enabled []string `json:"enabled_modules"`
}

// CapabilityGroups returns the capability map for the public descriptor.
// It exposes broad presence without leaking details.
func (r *Report) CapabilityGroups() map[string]map[string]any {
	caps := make(map[string]map[string]any)

	provs := make(map[string]any)
	for name, p := range r.Providers {
		if p.Installed {
			provs[name] = true
		}
	}
	if len(provs) > 0 {
		caps["providers"] = provs
	}

	caps["hosting"] = map[string]any{
		"shim": r.Hosting.LaunchHostShim,
	}

	caps["filesystem"] = map[string]any{
		"reflink": r.Filesystem.ReflinkSupported == "true",
	}

	// role module visibility
	modules := make(map[string]any)
	for _, m := range r.RoleProfile.Enabled {
		modules[m] = true
	}
	if len(modules) > 0 {
		caps["modules"] = modules
	}

	return caps
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
