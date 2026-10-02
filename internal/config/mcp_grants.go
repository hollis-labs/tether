package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalidMCPGrant identifies a declared upstream that cannot be granted.
var ErrInvalidMCPGrant = errors.New("invalid MCP grant")

// ValidateMCPGrant checks names without spawning upstreams or resolving secrets.
func (c *Catalog) ValidateMCPGrant(owner string, ids []string) error {
	var issues []error
	for _, id := range ids {
		enabled, exists := c.MCPServerEnabled[id]
		reason := "unknown upstream (names are case-sensitive)"
		if exists && enabled {
			continue
		}
		if exists {
			reason = "disabled upstream"
		}
		issues = append(issues, fmt.Errorf("%w: %s names %q: %s", ErrInvalidMCPGrant, owner, id, reason))
	}
	return errors.Join(issues...)
}

// ValidateMCPGrantEnv checks an explicit comma-separated environment grant.
// An empty value deliberately grants no upstreams.
func (c *Catalog) ValidateMCPGrantEnv(owner, value string) error {
	if value == "" {
		return nil
	}
	ids := strings.Split(value, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	return c.ValidateMCPGrant(owner, ids)
}

// ValidateMCPGrants checks every declared grant, including ones currently
// shadowed by a higher-precedence owner. Implicit defaults are not declarations.
func (c *Catalog) ValidateMCPGrants() error {
	var issues []error
	for id, p := range c.Projects {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("project %q mcp.servers", id), p.MCP.Servers))
	}
	for id, l := range c.Launches {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("launch %q mcp.servers", id), l.MCP.Servers))
		if value, set := l.Overrides.Env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("launch %q overrides.env.TETHER_MCP_SERVERS", id), value))
		}
	}
	for id, a := range c.Agents {
		if value, set := a.Env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("agent %q env.TETHER_MCP_SERVERS", id), value))
		}
		for provider, override := range a.ProviderOverrides {
			if value, set := override.Env["TETHER_MCP_SERVERS"]; set {
				issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("agent %q provider %q env.TETHER_MCP_SERVERS", id, provider), value))
			}
		}
	}
	for id, ids := range c.BootMCPGrants {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("boot profile %q mcp_servers", id), ids))
	}
	var messages []string
	for _, issue := range issues {
		if issue != nil {
			messages = append(messages, issue.Error())
		}
	}
	sort.Strings(messages)
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n%s", ErrInvalidMCPGrant, strings.Join(messages, "\n"))
}

// ValidateLaunchMCPGrants checks only owners in this launch's selected chain.
// Startup and doctor use ValidateMCPGrants to report all catalog declarations.
func (c *Catalog) ValidateLaunchMCPGrants(id string) error {
	l, exists := c.Launches[id]
	if !exists {
		return nil
	} // normal launch-reference validation reports this
	p := c.Projects[l.Project]
	a := c.Agents[l.Agent]
	var issues []error
	issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("launch %q mcp.servers", id), l.MCP.Servers))
	issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("project %q mcp.servers", l.Project), p.MCP.Servers))
	for owner, env := range map[string]map[string]string{
		fmt.Sprintf("launch %q overrides.env.TETHER_MCP_SERVERS", id):                   l.Overrides.Env,
		fmt.Sprintf("agent %q env.TETHER_MCP_SERVERS", l.Agent):                         a.Env,
		fmt.Sprintf("agent %q provider %q env.TETHER_MCP_SERVERS", l.Agent, l.Provider): a.ProviderOverrides[l.Provider].Env,
	} {
		if value, set := env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(owner, value))
		}
	}
	return errors.Join(issues...)
}
