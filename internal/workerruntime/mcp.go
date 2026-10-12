package workerruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type mcpEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}
type mcpConfig struct {
	Servers map[string]mcpEntry `json:"mcpServers"`
}

func ImportHubMCP(root, source string) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	selected := mcpConfig{Servers: map[string]mcpEntry{}}
	for _, name := range []string{"torque", "tesseract", "tether"} {
		entry, ok := config.Servers[name]
		if !ok || entry.Command == "" {
			return fmt.Errorf("configured hub MCP entry %s required", name)
		}
		selected.Servers[name] = entry
	}
	if err := validateHubMCP(selected); err != nil {
		return err
	}
	return SaveJSON(filepath.Join(root, "hub-mcp.json"), selected)
}
func validateHubMCP(config mcpConfig) error {
	entry, ok := config.Servers["tether"]
	if !ok || entry.Command != "ssh" {
		return fmt.Errorf("Tether MCP must run on the hub over SSH")
	}
	daemonOnly := false
	for _, arg := range entry.Args {
		if arg == "--daemon-only" {
			daemonOnly = true
		}
		if arg == "--proxy" {
			return fmt.Errorf("local/legacy Tether MCP proxy is not an outpost")
		}
	}
	if !daemonOnly {
		return fmt.Errorf("hub Tether MCP requires --daemon-only")
	}
	return nil
}
func (e *Executor) plantMCP(cwd string) error {
	b, err := os.ReadFile(filepath.Join(e.Root, "hub-mcp.json"))
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	if err = validateHubMCP(config); err != nil {
		return err
	}
	return plant(cwd, ".mcp.json", b)
}
