package mcpgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidSessionMCPPolicy = errors.New("invalid session MCP policy")

// SessionPolicy is the secret-free authority snapshot captured by the daemon
// after launch overrides. It never contains an environment, token or upstream
// credentials. Servers are fully resolved: empty is zero, not inheritance.
type SessionPolicy struct {
	SessionID     string   `json:"session_id"`
	AgentID       string   `json:"agent_id"`
	Servers       []string `json:"servers"`
	Profile       *string  `json:"profile,omitempty"`
	LaunchProfile *Profile `json:"launch_profile,omitempty"`
	DiscoveryMode *string  `json:"discovery_mode,omitempty"`
	Digest        string   `json:"digest"`
}

func (p SessionPolicy) hash() string {
	p.Digest = ""
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p SessionPolicy) Seal() SessionPolicy {
	p.Servers = append([]string{}, p.Servers...)
	if p.LaunchProfile != nil {
		value := CloneProfile(*p.LaunchProfile)
		p.LaunchProfile = &value
	}
	p.Digest = p.hash()
	return p
}

func (p SessionPolicy) Validate() error {
	if p.SessionID == "" || p.Servers == nil || p.Digest == "" || p.Digest != p.hash() {
		return fmt.Errorf("%w: invalid or missing snapshot; resume or relaunch the session", ErrInvalidSessionMCPPolicy)
	}
	if p.DiscoveryMode != nil {
		if err := ValidateMode(*p.DiscoveryMode); err != nil {
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
