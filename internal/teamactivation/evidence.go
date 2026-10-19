// Package teamactivation defines provider-owned observations, not execution grants.
package teamactivation

import (
	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

type Query struct {
	LaunchID  string `json:"launch_id"`
	SessionID string `json:"session_id,omitempty"`
}

type Launch = launchprofile.LaunchEvidence

type Session struct {
	ID              string             `json:"id"`
	State           string             `json:"state"`
	Pin             mesh.DefinitionRef `json:"admitted_pin"`
	LaunchID        string             `json:"launch_id"`
	PlanFingerprint string             `json:"plan_fingerprint"`
	Worker          string             `json:"worker,omitempty"`
	Project         string             `json:"project"`
	// Runtime observation is distinct from definition execution: the legacy
	// adapter does not execute the agentdef-v2 instruction/harness contract.
	RuntimeObserved    bool `json:"runtime_observed"`
	DefinitionExecuted bool `json:"definition_executed"`
}

type Evidence struct {
	AdmittedBaseline            *Launch             `json:"admitted_baseline,omitempty"`
	CurrentState                string              `json:"current_state"`
	Limitations                 []string            `json:"limitations"`
	PolicyDigest                string              `json:"policy_digest"`
	InstalledDefinitionPin      *mesh.DefinitionRef `json:"installed_definition_pin,omitempty"`
	DefinitionPermissionProfile string              `json:"definition_permission_profile,omitempty"`
	LoadedBaseline              *Launch             `json:"loaded_baseline,omitempty"`
	Admitted                    *Launch             `json:"admitted,omitempty"`
	Created                     *Launch             `json:"created,omitempty"`
	Launched                    *Launch             `json:"launched,omitempty"`
	BaselineState               string              `json:"baseline_state"`
	Version                     int                 `json:"version"`
	Mapping                     string              `json:"mapping"`
	Pin                         mesh.DefinitionRef  `json:"loaded_pin"`
	Loaded                      bool                `json:"loaded"`
	SourceVerified              bool                `json:"source_verified"`
	InstalledPolicyMatches      bool                `json:"installed_policy_matches"`
	Paused                      bool                `json:"paused"`
	Launch                      Launch              `json:"launch"`
	// A pin alone is never an attestation of effective catalog behavior.
	EvidenceDigest string   `json:"evidence_digest"`
	Divergence     []string `json:"divergence"`
	Session        *Session `json:"session,omitempty"`
}

// General removes selector-sensitive digests, including every nested aggregate.
// Private comparisons are already represented as closed divergence reasons.
func (e Evidence) General() Evidence {
	clean := func(in *Launch) *Launch {
		if in == nil {
			return nil
		}
		out := *in
		out.EffectiveSandboxPolicyDigest = ""
		out.InstalledSandboxFileDigest = ""
		out.InstalledSandboxPolicyDigest = ""
		out.Fingerprint = out.Digest()
		return &out
	}
	e.Launch = *clean(&e.Launch)
	e.LoadedBaseline = clean(e.LoadedBaseline)
	e.AdmittedBaseline = clean(e.AdmittedBaseline)
	e.Admitted = clean(e.Admitted)
	e.Created = clean(e.Created)
	e.Launched = clean(e.Launched)
	e.EvidenceDigest = "" // the private aggregate must never cross the general read boundary
	return e
}
