package mcpgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

var ErrInvalidSessionMCPPolicy = errors.New("invalid session MCP policy")

// SessionPolicy is the secret-free authority snapshot captured by the daemon
// after launch overrides. It never contains an environment, token or upstream
// credentials. Servers are fully resolved: empty is zero, not inheritance.
type SessionPolicy struct {
	UpstreamOwnership string   `json:"upstream_ownership,omitempty"`
	ExtractRefs       bool     `json:"extract_refs,omitempty"`
	SessionID         string   `json:"session_id"`
	AgentID           string   `json:"agent_id"`
	Servers           []string `json:"servers"`
	Profile           *string  `json:"profile,omitempty"`
	LaunchProfile     *Profile `json:"launch_profile,omitempty"`
	ToolProfile       *Profile `json:"tool_profile,omitempty"`
	DiscoveryMode     *string  `json:"discovery_mode,omitempty"`
	// EditableAgents is the catalog agent's editable_agents allowlist: the agent
	// ids this session's tether_agent_create / tether_agent_edit may touch. It is
	// saved with the policy at launch so a session reaching the hub over /mcp
	// keeps the restriction the ssh-launched `tether mcp --editable-agent` flags
	// carried. Seal sorts and de-duplicates it, so the same set always has the
	// same digest. Empty (omitted, which leaves the digest of every policy saved
	// before this field existed unchanged) means no per-agent restriction.
	EditableAgents []string `json:"editable_agents,omitempty"`
	Digest         string   `json:"digest"`
}

func (p SessionPolicy) hash() string {
	p.Digest = ""
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p SessionPolicy) Seal() SessionPolicy {
	p.Servers = append([]string{}, p.Servers...)
	p.EditableAgents = normalizeEditableAgents(p.EditableAgents)
	if p.ToolProfile != nil {
		value := CloneProfile(*p.ToolProfile)
		p.ToolProfile = &value
	}
	if p.LaunchProfile != nil {
		value := CloneProfile(*p.LaunchProfile)
		p.LaunchProfile = &value
	}
	p.Digest = p.hash()
	return p
}

// normalizeEditableAgents returns the allowlist sorted and without duplicates
// or empty ids; nil when nothing is left.
func normalizeEditableAgents(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func (p SessionPolicy) Validate() error {
	if p.SessionID == "" || p.Servers == nil || p.Digest == "" || p.Digest != p.hash() {
		return fmt.Errorf("%w: invalid or missing snapshot; resume or relaunch the session", ErrInvalidSessionMCPPolicy)
	}
	if !slices.Equal(p.EditableAgents, normalizeEditableAgents(p.EditableAgents)) {
		return fmt.Errorf("%w: editable_agents must be sorted, unique and non-empty", ErrInvalidSessionMCPPolicy)
	}
	if p.DiscoveryMode != nil {
		if err := ValidateMode(*p.DiscoveryMode); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSessionMCPPolicy, err)
		}
	}
	if p.ToolProfile != nil {
		if err := p.ToolProfile.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSessionMCPPolicy, err)
		}
	}
	if p.Profile != nil {
		if *p.Profile == "" || p.LaunchProfile == nil {
			return fmt.Errorf("%w: missing launch profile authority", ErrInvalidSessionMCPPolicy)
		}
		if err := p.LaunchProfile.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSessionMCPPolicy, err)
		}
	}
	return nil
}
