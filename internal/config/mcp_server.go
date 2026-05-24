package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// MCPServerEntry describes one upstream MCP server in the catalog.
// Files live at <catalogDir>/mcp-servers/*.yaml.
type MCPServerEntry struct {
	ID        string            `yaml:"id"`
	Transport string            `yaml:"transport"` // "stdio" | "sse"
	Command   string            `yaml:"command"`   // stdio: binary path
	Args      []string          `yaml:"args"`      // stdio: arguments
	Env       map[string]string `yaml:"env"`       // env vars; values support ${VAR} expansion
	URL       string            `yaml:"url"`       // sse: endpoint URL
	Token     string            `yaml:"token"`     // bearer token or ${VAR} ref
	Scopes    []string          `yaml:"scopes"`
	Enabled   *bool             `yaml:"enabled"` // nil → defaults to true
	Tags      []string          `yaml:"tags"`
}

// IsEnabled returns true when the entry should be loaded. A missing enabled
// field (nil pointer) is treated as true.
func (e *MCPServerEntry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

var envVarRE = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnvRefs replaces ${VAR} tokens with os.Getenv(VAR) values.
func expandEnvRefs(s string) string {
	return envVarRE.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-1] // strip ${ and }
		return os.Getenv(key)
	})
}

// LoadMCPServers reads all *.yaml files from <catalogDir>/mcp-servers/,
// parses them as MCPServerEntry, expands ${VAR} references, and returns
// the enabled entries. A missing directory is silently treated as empty.
func LoadMCPServers(catalogDir string) ([]MCPServerEntry, error) {
	entries, err := LoadMCPServerCatalog(catalogDir)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, entry := range entries {
		if entry.IsEnabled() {
			out = append(out, entry)
		}
	}
	return out, nil
}

// LoadMCPServerCatalog reads all upstream MCP server catalog entries,
// including disabled entries. GUI/config surfaces use this so disabled
// servers remain visible and can be re-enabled.
func LoadMCPServerCatalog(catalogDir string) ([]MCPServerEntry, error) {
	dir := filepath.Join(catalogDir, "mcp-servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp-servers dir: %w", err)
	}

	var out []MCPServerEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		var entry MCPServerEntry
		if err := yaml.Unmarshal(b, &entry); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		// Expand ${VAR} references at load time.
		entry.URL = expandEnvRefs(entry.URL)
		entry.Token = expandEnvRefs(entry.Token)
		expanded := make(map[string]string, len(entry.Env))
		for k, v := range entry.Env {
			expanded[k] = expandEnvRefs(v)
		}
		entry.Env = expanded

		out = append(out, entry)
	}
	return out, nil
}
