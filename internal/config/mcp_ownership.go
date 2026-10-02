package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
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
	if value == "" {
		return MCPUpstreamsLegacy, nil
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
		Daemon struct {
			Ownership string `yaml:"mcp_upstream_ownership"`
		} `yaml:"daemon"`
	}
	if err := yaml.Unmarshal(raw, &selected); err != nil {
		return "", fmt.Errorf("read MCP ownership setting: %w", err)
	}
	return (DaemonConfig{MCPUpstreams: selected.Daemon.Ownership}).MCPUpstreamOwnership()
}
