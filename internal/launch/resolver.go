package launch

import (
	"fmt"
	"os"

	"github.com/chrispian/agent-mux/internal/config"
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

	env := map[string]string{}
	for _, k := range prov.Env.Passthrough {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range l.Overrides.Env {
		env[k] = v
	}

	writeHome := l.Workspace.WriteHome
	if writeHome == "" {
		writeHome = proj.Workspace.SessionRoot
	}

	return &Plan{
		LaunchID:   l.ID,
		ProjectID:  proj.ID,
		AgentID:    agent.ID,
		ProviderID: prov.ID,
		RepoRoot:   config.Expand(proj.RepoRoot),
		WriteHome:  config.Expand(writeHome),
		Command:    prov.Command,
		Args:       prov.Args,
		Env:        env,
		BootPrompt: boot,
		BootMode:   prov.Bootstrap.Mode,
	}, nil
}
