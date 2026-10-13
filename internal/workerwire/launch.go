package workerwire

import (
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

type Workspace struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Subdir string `json:"subdir,omitempty"`
}

// FeatureSessionCred is advertised in a worker's hello when it can plant a
// session-bound hub credential. The hub sends LaunchSpec.Credential only to
// workers that advertise it, so older workers keep their static MCP entry.
const FeatureSessionCred = "session-cred"

type LaunchSpec struct {
	Session  string `json:"session"`
	Actor    string `json:"actor"`
	Provider string `json:"provider"`
	// PermissionMode is the resolved agent permission posture ("bypass" or "default"). A worker has no
	// human to answer tool-permission prompts, so "bypass" must reach the provider as its own flag.
	PermissionMode string                     `json:"permission_mode,omitempty"`
	RuntimeKind    string                     `json:"runtime_kind"`
	Args           []string                   `json:"args"`
	Env            map[string]string          `json:"env"`
	BootPrompt     string                     `json:"boot_prompt"`
	BootMode       string                     `json:"boot_mode"`
	BootFiles      map[string]string          `json:"boot_files,omitempty"`
	NativeFiles    []launchprofile.NativeFile `json:"native_files,omitempty"`
	Workspace      Workspace                  `json:"workspace"`
	Sandbox        *sandbox.Profile           `json:"sandbox,omitempty"`
	// Credential is the session-bound hub bearer. It is a secret: the worker writes it to a 0600 file and
	// never to process arguments, the child environment, logs or the output stream. Only sent to workers
	// that advertise FeatureSessionCred.
	Credential string `json:"credential,omitempty"`
	// MCPScopes are the scopes the credential carries; the planted tether MCP entry requests the same set.
	MCPScopes []string `json:"mcp_scopes,omitempty"`
	// MCPServers is the resolved catalog mcp-servers grant (agent/launch cascade). The worker plants the
	// tether entry plus only these entries from its hub-mcp.json. Nil (a hub that predates the field)
	// plants every imported entry; an empty non-nil list plants tether alone.
	MCPServers []string `json:"mcp_servers"`
}
