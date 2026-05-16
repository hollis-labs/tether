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

	"github.com/hollis-labs/tether/internal/agentops"
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
	Long: `Create a new agent YAML file in one of the three discovery layers.

The --scope flag selects which layer the file is written to, which determines
which launches can reference the agent:

  project   <repo>/.tether/agents/      Visible to launches for that repo only.
                                        Commit the file to share it with the
                                        team. Best for repo-specific agents
                                        (auditors, builders for one codebase).
                                        (default)
  user      ~/.tether/agents/           Visible to all of your launches on this
                                        machine, across every project. Use for
                                        personal, cross-project agents.
  system    <catalog>/agents/           The curated, shared system catalog.

Launch validation resolves agents across all three layers, with project
overriding user overriding system — so a launch may reference an agent
created at any scope. Pick the narrowest scope that fits: project for
repo-specific work, user for your own cross-project agents, system only for
the deliberately-curated shared catalog.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		layer, err := agentops.ParseScope(agentsCreateScope)
		if err != nil {
			return err
		}
		root, err := layerRoot(layer)
		if err != nil {
			return err
		}
		path, err := agentops.Create(root, id, agentops.Params{
			Name:         agentsCreateName,
			SystemPrompt: agentsCreateSystem,
			AgentPrompt:  agentsCreateAgentPrompt,
		})
		if err != nil {
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
// (catalogPath), the user layer (~/.tether/), and the project layer
// (./.tether/, relative to the daemon's CWD).
func agentDiscovery() (*config.LayeredCatalog, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	layers := config.DefaultLayers(catalogPath, cwd)
	return config.Discover(layers)
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
		return filepath.Join(home, ".tether"), nil
	case config.LayerProject:
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".tether"), nil
	}
	return "", fmt.Errorf("unknown layer: %s", layer)
}

func init() {
	agentsCreateCmd.Flags().StringVar(&agentsCreateScope, "scope", "project", "discovery layer to write the agent into: project (this repo) | user (all your projects) | system (shared catalog) — see 'mux agents create --help'")
	agentsCreateCmd.Flags().StringVar(&agentsCreateName, "name", "", "human-readable name (defaults to id)")
	agentsCreateCmd.Flags().StringVar(&agentsCreateSystem, "system-prompt", "", "agent's system prompt (inline string)")
	agentsCreateCmd.Flags().StringVar(&agentsCreateAgentPrompt, "agent-prompt", "", "agent's persona prompt (inline string)")

	agentsCmd.AddCommand(agentsListCmd, agentsCreateCmd, agentsEditCmd, agentsShowCmd)
}
