package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	MCPUpstreamsLegacy = "legacy_proxy"
	MCPUpstreamsDaemon = "daemon"
)

// MCPUpstreamOwnership resolves at startup, doctor and launch boundaries, never
// during shared catalog loading: malformed settings must not strand clients.
func (c DaemonConfig) MCPUpstreamOwnership() (string, error) {
	value := strings.TrimSpace(c.MCPUpstreams)
	if c.MCPUpstreams == "" && !c.mcpOwnershipPresent && !c.mcpOwnershipInvalid {
		return MCPUpstreamsLegacy, nil
	}
	if c.mcpOwnershipInvalid {
		return "", fmt.Errorf("daemon.mcp_upstream_ownership must be legacy_proxy or daemon, not null or a non-string value")
	}
	switch value {
	case MCPUpstreamsLegacy, MCPUpstreamsDaemon:
		return value, nil
	default:
		return "", fmt.Errorf("unknown daemon.mcp_upstream_ownership %q; choose legacy_proxy or daemon", value)
	}
}

// ReadMCPUpstreamOwnership re-reads only the planting selector for a new launch.
// Endpoint enablement/listener identity remain daemon-start configuration.
// The fallback supports an embedded catalog without a global.yaml file.
func ReadMCPUpstreamOwnership(root string, fallback DaemonConfig) (string, error) {
	raw, err := os.ReadFile(filepath.Join(Expand(root), "global.yaml")) //nolint:gosec // operator-owned catalog path
	if os.IsNotExist(err) {
		return fallback.MCPUpstreamOwnership()
	}
	if err != nil {
		return "", fmt.Errorf("read MCP ownership setting: %w", err)
	}
	var selected struct {
		Daemon DaemonConfig `yaml:"daemon"`
	}
	if err := yaml.Unmarshal(raw, &selected); err != nil {
		return "", fmt.Errorf("read MCP ownership setting: %w", err)
	}
	return selected.Daemon.MCPUpstreamOwnership()
}

// UnmarshalYAML retains ownership presence and invalid values for validation at
// startup/doctor/launch. Other commands can still load the catalog to stop work.
func (c *DaemonConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain DaemonConfig
	// Decode nodes rather than string values so explicit null/non-string values
	// survive shared loading. YAML merge keys are resolved before selection.
	var fields map[string]yaml.Node
	if err := node.Decode(&fields); err != nil {
		return err
	}
	filtered := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	ownership, present := fields["mcp_upstream_ownership"]
	for ownership.Kind == yaml.AliasNode {
		ownership = *ownership.Alias
	}
	invalid := present && (ownership.Kind != yaml.ScalarNode || ownership.Tag != "!!str")
	value := ""
	if present && !invalid {
		value = ownership.Value
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if key != "mcp_upstream_ownership" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		field := fields[key]
		filtered.Content = append(filtered.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &field)
	}
	var decoded plain
	if err := filtered.Decode(&decoded); err != nil {
		return err
	}
	*c = DaemonConfig(decoded)
	c.MCPUpstreams = value
	c.mcpOwnershipPresent, c.mcpOwnershipInvalid = present, invalid
	return nil
}
