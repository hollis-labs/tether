package workerwire

import (
	"github.com/hollis-labs/libs/harness/core/sandbox"
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

// FeatureExtractRefs means the worker plants the hub's extraction policy and
// canonical session id on its credential-bearing MCP relay.
const FeatureExtractRefs = "extract-refs"

// FeatureWorkerResume is advertised by a worker that honors
// LaunchSpec.ResumeSessionID (provider-native resume in the same worker
// workspace). The hub resumes on a worker only when it is advertised.
const FeatureWorkerResume = "worker-resume"

// FeatureEditableAgents is advertised by a worker that plants
// LaunchSpec.EditableAgents as --editable-agent on the tether MCP entry. The
// hub refuses to launch an agent that declares editable_agents on a worker
// without it: an older worker would drop the allowlist and leave the session
// unrestricted.
const FeatureEditableAgents = "editable-agents"

// FeatureAgentEvents is advertised by a worker that plants Claude lifecycle hooks (`tether-worker hook claude`) when
// LaunchSpec.AgentEvents is set (CW-20261014-0143). The hub sets the flag only for workers that advertise it.
const FeatureAgentEvents = "agent-events"

// FeatureAgentToken is advertised in a worker's hello when it can request
// short-lived scoped agent tokens from the hub's issuer, keep them in
// per-session 0600 token files and refresh them in place. The hub sends
// LaunchSpec.AgentTokens only to workers that advertise it.
const FeatureAgentToken = "agent-token"

// AgentTokenSpec is one token the outpost keeps for a session: audience (one
// app) and the scopes the launch policy grants.
type AgentTokenSpec struct {
	Aud    string   `json:"aud"`
	Scopes []string `json:"scopes"`
}

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
	// ExtractRefs is the resolved hub policy, off by default. Enabling it requires
	// FeatureExtractRefs and a session credential; old workers must not drop it.
	ExtractRefs bool `json:"extract_refs,omitempty"`
	// EditableAgents are the agent ids the session's tether_agent_create /
	// tether_agent_edit may touch, planted as --editable-agent on the tether MCP
	// entry. Empty means no per-agent restriction. Only sent to workers that
	// advertise FeatureEditableAgents.
	EditableAgents []string `json:"editable_agents,omitempty"`
	// MCPServers is the resolved catalog mcp-servers grant (agent/launch cascade). The worker plants the
	// tether entry plus only these entries from its hub-mcp.json. Nil (a hub that predates the field)
	// plants every imported entry; an empty non-nil list plants tether alone.
	MCPServers []string `json:"mcp_servers"`
	// MCPPolicy says the hub saved this session's MCP policy (servers, profile, tools, extract_refs and the
	// editable_agents allowlist) before sending the launch, so the hub's /mcp gateway can serve the session and
	// enforces that allowlist itself. A worker plants the /mcp relay only when it is set; a hub that predates it
	// never sets it, and the worker then needs the degraded ssh fallback.
	MCPPolicy bool `json:"mcp_policy,omitempty"`
	// AgentEvents asks the worker to plant Claude lifecycle hooks that record content-free activity events to the
	// session's local spool (CW-20261014-0143). Default off; only sent to workers that advertise FeatureAgentEvents.
	AgentEvents bool `json:"agent_events,omitempty"`
	// ResumeSessionID is the provider's own conversation id to continue (claude --resume). Set only for a
	// hub-approved resume on the same worker that ran the source session.
	ResumeSessionID string `json:"resume_session_id,omitempty"`

	// AgentTokens are the issued-JWT token files the outpost maintains for the
	// session (see FeatureAgentToken). The hub registers the same set as the
	// session's launch grant, which bounds what the outpost can mint.
	AgentTokens []AgentTokenSpec `json:"agent_tokens,omitempty"`
}
