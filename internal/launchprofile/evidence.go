package launchprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SandboxStructure summarizes policy without disclosing path selectors.
type SandboxStructure struct {
	ReadCount          int  `json:"read_count"`
	WriteCount         int  `json:"write_count"`
	DenyCount          int  `json:"deny_count"`
	ProtectCount       int  `json:"protect_count"`
	Net                bool `json:"network"`
	Loopback           bool `json:"loopback"`
	Subprocess         bool `json:"subprocess"`
	HostFilesystem     bool `json:"host_filesystem"`
	DenyGUI            bool `json:"deny_gui"`
	DenyServiceManager bool `json:"deny_service_manager"`
}

// LaunchEvidence commits to authority and placement selectors, excluding secret-bearing values.
// It does not claim execution of an agentdef-v2 definition.
type LaunchEvidence struct {
	PolicyDigest                 string           `json:"policy_digest,omitempty"`
	CatalogGenerationKnown       bool             `json:"catalog_generation_known"`
	PlacementResolved            bool             `json:"placement_resolved"`
	RecordedAt                   string           `json:"recorded_at,omitempty"`
	CatalogGeneration            uint64           `json:"catalog_reload_generation"`
	InstalledSandboxFileDigest   string           `json:"installed_sandbox_file_digest,omitempty"`
	InstalledSandboxPolicyDigest string           `json:"installed_sandbox_policy_digest,omitempty"`
	SandboxStructure             SandboxStructure `json:"sandbox_structure"`
	EffectiveSandboxPolicyDigest string           `json:"effective_sandbox_policy_digest,omitempty"`
	SandboxEnforcement           string           `json:"sandbox_enforcement"`
	ObservedAt                   string           `json:"observed_at"`
	BootProfileID                string           `json:"boot_profile_id,omitempty"`
	Version                      string           `json:"version"`
	ExecutionBasis               string           `json:"execution_basis"`
	LaunchID                     string           `json:"launch_id"`
	CatalogAgent                 string           `json:"catalog_agent"`
	Provider                     string           `json:"provider"`
	Brand                        string           `json:"brand"`
	Runtime                      string           `json:"runtime"`
	CatalogModel                 string           `json:"catalog_model,omitempty"`
	Worker                       string           `json:"worker,omitempty"`
	Project                      string           `json:"project"`
	PermissionMode               string           `json:"permission_mode"`
	Sandbox                      string           `json:"sandbox"`
	SandboxDigest                string           `json:"sandbox_structure_digest"`
	MCPServers                   []string         `json:"mcp_servers"`
	GrantedScopes                []string         `json:"granted_scopes,omitempty"`
	ScopeResolution              string           `json:"scope_resolution"`
	EnvironmentKeys              []string         `json:"environment_keys,omitempty"`
	Route                        *Route           `json:"route,omitempty"`
	Lifecycle                    *LifecyclePolicy `json:"lifecycle,omitempty"`
	Placement                    string           `json:"placement"`
	Fingerprint                  string           `json:"fingerprint"`
}

func (e LaunchEvidence) Digest() string {
	// Stage observations remain visible but are not comparable plan facts.
	e.ScopeResolution = ""
	e.GrantedScopes = nil
	e.SandboxEnforcement = ""
	e.PlacementResolved = false
	e.Fingerprint = ""
	e.ObservedAt = ""
	e.PolicyDigest = ""
	e.RecordedAt = ""
	e.CatalogGeneration = 0
	e.CatalogGenerationKnown = false
	e.EffectiveSandboxPolicyDigest = ""
	e.InstalledSandboxFileDigest = ""
	e.InstalledSandboxPolicyDigest = ""
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
