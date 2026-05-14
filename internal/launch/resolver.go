package launch

import (
	"fmt"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
)

type Input struct {
	LaunchID    string
	CatalogRoot string
}

func Resolve(cat *config.Catalog, in Input) (*Plan, error) {
	l, ok := cat.Launches[in.LaunchID]
	if !ok {
		return nil, fmt.Errorf("launch %q not found", in.LaunchID)
	}
	proj := cat.Projects[l.Project]
	agent := cat.Agents[l.Agent]
	prov := cat.Providers[l.Provider]

	var fragments []string
	if l.Prompt.IncludeProjectBoot {
		fragments = append(fragments, proj.BootFragments...)
	}
	if l.Prompt.IncludeAgentBoot {
		fragments = append(fragments, agent.BootFragments...)
	}
	if l.Prompt.IncludeKnowledgeBase {
		fragments = append(fragments, proj.KnowledgeBase...)
	}
	boot, err := Compose(in.CatalogRoot, fragments)
	if err != nil {
		return nil, fmt.Errorf("compose boot prompt: %w", err)
	}
	if prov.Bootstrap.PromptPrefix != "" {
		boot = prov.Bootstrap.PromptPrefix + "\n" + boot
	}

	// Carry only the explicit overrides into the plan. The adapter composes
	// the effective child env at launch time per prov.Env.Mode, so parent
	// values are never materialized into plan.Env (and thus never persisted
	// in launch_plans). See internal/provider/env.go.
	overrides := map[string]string{}
	for k, v := range l.Overrides.Env {
		overrides[k] = v
	}

	// Resolve MCP server filter: project wins over launch. Injected as
	// MUX_MCP_SERVERS so the spawned agent's mux mcp --proxy process picks it
	// up without requiring per-agent ~/.claude.json changes.
	mcpServers := proj.MCP.Servers
	if len(mcpServers) == 0 {
		mcpServers = l.MCP.Servers
	}
	if len(mcpServers) > 0 {
		overrides["MUX_MCP_SERVERS"] = strings.Join(mcpServers, ",")
	}

	mode := prov.Env.Mode
	if mode == "" {
		mode = "merge"
	}

	writeHome := l.Workspace.WriteHome
	if writeHome == "" {
		writeHome = proj.Workspace.SessionRoot
	}

	return &Plan{
		LaunchID:       l.ID,
		ProjectID:      proj.ID,
		LogicalAgentID: agent.ID,
		ProviderID:     prov.ID,
		ProviderBrand:  prov.ProviderBrand(),
		RuntimeKind:    prov.EffectiveRuntimeKind(),
		RepoRoot:       config.Expand(proj.RepoRoot),
		WriteHome:      config.Expand(writeHome),
		Command:        prov.Command,
		Args:           prov.Args,
		Env:            overrides,
		EnvMode:        mode,
		EnvPassthrough: prov.Env.Passthrough,
		EnvRedact:      prov.Env.Redact,
		BootPrompt:     boot,
		BootMode:       prov.Bootstrap.Mode,
	}, nil
}
