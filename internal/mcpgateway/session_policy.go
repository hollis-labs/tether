package mcpgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SessionPolicy is the secret-free authority snapshot captured by the daemon
// after launch overrides. It never contains an environment, token or upstream
// credentials. Servers are fully resolved: empty is zero, not inheritance.
type SessionPolicy struct {
	SessionID     string   `json:"session_id"`
	AgentID       string   `json:"agent_id"`
	Servers       []string `json:"servers"`
	Profile       *string  `json:"profile,omitempty"`
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
	p.Digest = p.hash()
	return p
}

func (p SessionPolicy) Validate() error {
	if p.SessionID == "" || p.Servers == nil || p.Digest == "" || p.Digest != p.hash() {
		return fmt.Errorf("invalid or missing session MCP policy snapshot; resume or relaunch the session")
	}
	if p.DiscoveryMode != nil {
		if err := ValidateMode(*p.DiscoveryMode); err != nil {
			return err
		}
	}
	if p.Profile != nil && *p.Profile == "" {
		return fmt.Errorf("empty session MCP profile")
	}
	return nil
}
