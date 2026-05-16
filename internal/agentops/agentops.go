// Package agentops implements create / update operations on agent YAML files
// across Tether's system / user / project discovery layers. Both the
// `mux agents` CLI and the MCP adapter route through this package so the two
// surfaces stay behavior-identical. Reads go through config.Discover; this
// package owns the writes.
package agentops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/config"
)

// ParseScope maps a --scope flag / scope argument to a discovery Layer. An
// empty string defaults to the project layer — the narrowest scope, and the
// one that keeps an agent committed alongside the repo it serves.
func ParseScope(s string) (config.Layer, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "project":
		return config.LayerProject, nil
	case "user":
		return config.LayerUser, nil
	case "system":
		return config.LayerSystem, nil
	default:
		return 0, fmt.Errorf("unknown scope %q (want one of: project, user, system)", s)
	}
}

// Params carries the writable fields of an agent. On Create every field seeds
// the new file. On Update an empty string leaves the field unchanged, and a
// non-nil slice (even an empty one) replaces the existing value — matching the
// list-replace semantics used elsewhere for agent merges.
type Params struct {
	Name         string
	Roles        []string
	Skills       []string
	SystemPrompt string
	AgentPrompt  string
}

// ValidID reports whether id is usable as an agent ID and filename — a single
// path segment with no separators or traversal.
func ValidID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." {
		return false
	}
	return filepath.Base(id) == id && !strings.ContainsAny(id, `/\`)
}

// Create writes a brand-new agent file at <layerRoot>/agents/<id>.yaml. It
// returns an error if a file already exists there — callers must edit via
// Update instead of clobbering. Returns the written path.
func Create(layerRoot, id string, p Params) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid agent id %q: must be a single name with no path separators", id)
	}
	path := filepath.Join(layerRoot, "agents", id+".yaml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("agent already exists at %s", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	name := p.Name
	if name == "" {
		name = id
	}
	a := config.Agent{
		ID:           id,
		Name:         name,
		Roles:        p.Roles,
		Skills:       p.Skills,
		SystemPrompt: p.SystemPrompt,
		AgentPrompt:  p.AgentPrompt,
	}
	if err := writeAgent(path, a); err != nil {
		return "", err
	}
	return path, nil
}

// Update loads the agent file at path, applies the non-empty fields of p, and
// rewrites the file in place. Returns the resulting agent.
func Update(path string, p Params) (config.Agent, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
	if err != nil {
		return config.Agent{}, err
	}
	var a config.Agent
	if err := yaml.Unmarshal(data, &a); err != nil {
		return config.Agent{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.Name != "" {
		a.Name = p.Name
	}
	if p.Roles != nil {
		a.Roles = p.Roles
	}
	if p.Skills != nil {
		a.Skills = p.Skills
	}
	if p.SystemPrompt != "" {
		a.SystemPrompt = p.SystemPrompt
	}
	if p.AgentPrompt != "" {
		a.AgentPrompt = p.AgentPrompt
	}
	if err := writeAgent(path, a); err != nil {
		return config.Agent{}, err
	}
	return a, nil
}

func writeAgent(path string, a config.Agent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	body, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}
