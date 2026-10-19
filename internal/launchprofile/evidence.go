package launchprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// LaunchEvidence commits only to authority/placement selectors. It deliberately
// excludes environment values, argv, instructions, injected files and user paths.
// It is not a claim that an agentdef-v2 definition was executed.
type LaunchEvidence struct {
	Version         string           `json:"version"`
	ExecutionBasis  string           `json:"execution_basis"`
	LaunchID        string           `json:"launch_id"`
	CatalogAgent    string           `json:"catalog_agent"`
	Provider        string           `json:"provider"`
	Brand           string           `json:"brand"`
	Runtime         string           `json:"runtime"`
	CatalogModel    string           `json:"catalog_model,omitempty"`
	Worker          string           `json:"worker,omitempty"`
	Project         string           `json:"project"`
	PermissionMode  string           `json:"permission_mode"`
	Sandbox         string           `json:"sandbox"`
	SandboxDigest   string           `json:"sandbox_digest"`
	MCPServers      []string         `json:"mcp_servers"`
	GrantedScopes   []string         `json:"granted_scopes,omitempty"`
	ScopeResolution string           `json:"scope_resolution"`
	EnvironmentKeys []string         `json:"environment_keys,omitempty"`
	Route           *Route           `json:"route,omitempty"`
	Lifecycle       *LifecyclePolicy `json:"lifecycle,omitempty"`
	Placement       string           `json:"placement"`
	Fingerprint     string           `json:"fingerprint"`
}

func (e LaunchEvidence) Digest() string {
	e.Fingerprint = ""
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
