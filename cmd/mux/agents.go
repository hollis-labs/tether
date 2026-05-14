package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/config"
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Agent commands",
}

var agentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured agents across discovery layers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cat, err := agentDiscovery()
		if err != nil {
			return err
		}
		// Sort by ID for stable output.
		ids := make([]string, 0, len(cat.Agents))
		for id := range cat.Agents {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Printf("%-24s  %-8s  %s\n", "ID", "LAYER", "NAME")
		for _, id := range ids {
			la := cat.Agents[id]
			fmt.Printf("%-24s  %-8s  %s\n", la.Agent.ID, la.Layer.String(), la.Agent.Name)
		}
		return nil
	},
}

var (
	agentsCreateScope       string
	agentsCreateName        string
	agentsCreateSystem      string
	agentsCreateAgentPrompt string
)

var agentsCreateCmd = &cobra.Command{
	Use:   "create <id>",
	Short: "Create a new agent YAML in the chosen discovery layer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		layer, err := resolveScope(agentsCreateScope)
		if err != nil {
			return err
		}
		root, err := layerRoot(layer)
		if err != nil {
			return err
		}
		path := filepath.Join(root, "agents", id+".yaml")
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("agent already exists at %s — edit with `mux agents edit %s` (or remove the file first)", path, id)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		name := agentsCreateName
		if name == "" {
			name = id
		}
		a := config.Agent{
			ID:           id,
			Name:         name,
			SystemPrompt: agentsCreateSystem,
			AgentPrompt:  agentsCreateAgentPrompt,
		}
		body, err := yaml.Marshal(a)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%s layer)\n", path, layer.String())
		return nil
	},
}

var agentsEditCmd = &cobra.Command{
	Use:   "edit <id>",
	Short: "Open the agent YAML in $EDITOR (or print its path if EDITOR is unset)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		cat, err := agentDiscovery()
		if err != nil {
			return err
		}
		la, ok := cat.Agents[id]
		if !ok {
			return fmt.Errorf("agent %q not found in any discovery layer", id)
		}
		editor := strings.TrimSpace(os.Getenv("EDITOR"))
		if editor == "" {
			fmt.Println(la.Path)
			return nil
		}
		c := exec.Command(editor, la.Path) //nolint:gosec // G204: $EDITOR is an operator-controlled env var by convention
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	},
}

var agentsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print the resolved agent YAML with its discovery-layer annotation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		cat, err := agentDiscovery()
		if err != nil {
			return err
		}
		la, ok := cat.Agents[id]
		if !ok {
			return fmt.Errorf("agent %q not found in any discovery layer", id)
		}
		fmt.Printf("# id:    %s\n# layer: %s\n# path:  %s\n\n", la.Agent.ID, la.Layer.String(), la.Path)
		body, err := yaml.Marshal(la.Agent)
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(body); err != nil {
			return err
		}
		return nil
	},
}

// agentDiscovery returns the LayeredCatalog assembled from the system catalog
// (catalogPath), the user layer (~/.agent-mux/), and the project layer
// (./.agent-mux/, relative to the daemon's CWD).
func agentDiscovery() (*config.LayeredCatalog, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	layers := config.DefaultLayers(catalogPath, cwd)
	return config.Discover(layers)
}

// resolveScope maps the --scope flag to a Layer. Defaults to user when empty.
func resolveScope(s string) (config.Layer, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "user":
		return config.LayerUser, nil
	case "project":
		return config.LayerProject, nil
	case "system":
		return config.LayerSystem, nil
	default:
		return 0, fmt.Errorf("unknown scope %q (want one of: user, project, system)", s)
	}
}

// layerRoot returns the on-disk root for the given layer.
func layerRoot(layer config.Layer) (string, error) {
	switch layer {
	case config.LayerSystem:
		return config.Expand(catalogPath), nil
	case config.LayerUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".agent-mux"), nil
	case config.LayerProject:
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".agent-mux"), nil
	}
	return "", fmt.Errorf("unknown layer: %s", layer)
}

func init() {
	agentsCreateCmd.Flags().StringVar(&agentsCreateScope, "scope", "user", "discovery layer to write into: user | project | system")
	agentsCreateCmd.Flags().StringVar(&agentsCreateName, "name", "", "human-readable name (defaults to id)")
	agentsCreateCmd.Flags().StringVar(&agentsCreateSystem, "system-prompt", "", "agent's system prompt (inline string)")
	agentsCreateCmd.Flags().StringVar(&agentsCreateAgentPrompt, "agent-prompt", "", "agent's persona prompt (inline string)")

	agentsCmd.AddCommand(agentsListCmd, agentsCreateCmd, agentsEditCmd, agentsShowCmd)
}
