// Package agentops implements create / update operations on agent YAML files
// across Tether's system / user / project discovery layers. Both the
// `tether agents` CLI and the MCP adapter route through this package so the two
// surfaces stay behavior-identical. Reads go through config.Discover; this
// package owns the writes.
package agentops

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// ErrExists is returned by Create when an agent file already exists at the
// target path. Callers should classify it distinctly from I/O failures — an
// already-exists is a caller/action error, not an internal fault.
var ErrExists = errors.New("agent already exists")

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

// PathFor is where Create writes the agent id under layerRoot:
// <layerRoot>/agents/<id>.yaml. A caller that must decide whether a write is
// allowed asks for the path first.
func PathFor(layerRoot, id string) string {
	return filepath.Join(layerRoot, "agents", id+".yaml")
}

// Create writes a brand-new agent file at <layerRoot>/agents/<id>.yaml. It
// returns an error if a file already exists there — callers must edit via
// Update instead of clobbering. Returns the written path.
func Create(layerRoot, id string, p Params) (string, error) {
	if !ValidID(id) {
		return "", invalidIDError(id)
	}
	path := PathFor(layerRoot, id)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%w at %s", ErrExists, path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	a := newAgent(id, p)
	if err := writeAgent(path, a); err != nil {
		return "", err
	}
	return path, nil
}

// Update loads the agent file at path, applies the populated fields of p, and
// rewrites the file in place. Returns the resulting agent.
//
// Update is a partial patch, not a full replace:
//   - An empty string leaves a scalar field (Name/SystemPrompt/AgentPrompt)
//     unchanged — scalars cannot be cleared to "" through Update.
//   - A non-nil slice (even empty) replaces Roles/Skills; a nil slice leaves
//     the existing list untouched. This is how a caller clears a list.
//
// Comments, key order and keys not modeled by launchprofile.LaunchProfile are
// preserved; only the patched keys change.
func Update(path string, p Params) (launchprofile.LaunchProfile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	a, body, err := patchAgent(path, data, p)
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	return a, nil
}

func invalidIDError(id string) error {
	return fmt.Errorf("invalid agent id %q: must be a single name with no path separators", id)
}

// newAgent is the agent file Create seeds from p.
func newAgent(id string, p Params) launchprofile.LaunchProfile {
	name := p.Name
	if name == "" {
		name = id
	}
	return launchprofile.LaunchProfile{
		ID:           id,
		Name:         name,
		Roles:        p.Roles,
		Skills:       p.Skills,
		SystemPrompt: p.SystemPrompt,
		AgentPrompt:  p.AgentPrompt,
	}
}

// patchAgent applies the populated fields of p to the agent file in data (read
// from path) and returns the resulting agent and the file body to write. It
// edits the YAML node tree, so comments, key order and keys that
// launchprofile.LaunchProfile does not model survive the edit; only the
// patched keys change.
func patchAgent(path string, data []byte, p Params) (launchprofile.LaunchProfile, []byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return launchprofile.LaunchProfile{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 { // empty file
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return launchprofile.LaunchProfile{}, nil, fmt.Errorf("parse %s: agent file is not a YAML mapping", path)
	}
	if p.Name != "" {
		setScalar(root, "name", p.Name)
	}
	if p.Roles != nil {
		setList(root, "roles", p.Roles)
	}
	if p.Skills != nil {
		setList(root, "skills", p.Skills)
	}
	if p.SystemPrompt != "" {
		setScalar(root, "system_prompt", p.SystemPrompt)
	}
	if p.AgentPrompt != "" {
		setScalar(root, "agent_prompt", p.AgentPrompt)
	}
	var a launchprofile.LaunchProfile
	if err := root.Decode(&a); err != nil {
		return launchprofile.LaunchProfile{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return launchprofile.LaunchProfile{}, nil, err
	}
	if err := enc.Close(); err != nil {
		return launchprofile.LaunchProfile{}, nil, err
	}
	return a, buf.Bytes(), nil
}

// setValue replaces the value node of key in mapping m, or appends the pair.
func setValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v.HeadComment, v.LineComment, v.FootComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment, m.Content[i+1].FootComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

func setScalar(m *yaml.Node, key, val string) {
	setValue(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
}

func setList(m *yaml.Node, key string, vals []string) {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range vals {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	setValue(m, key, n)
}

func marshalAgent(a launchprofile.LaunchProfile) ([]byte, error) { return yaml.Marshal(a) }

func writeAgent(path string, a launchprofile.LaunchProfile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	body, err := marshalAgent(a)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}
