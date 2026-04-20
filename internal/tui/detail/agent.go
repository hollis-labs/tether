package detail

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// AgentScreen renders a config.Agent.
type AgentScreen struct {
	base
	a config.Agent
}

func NewAgentScreen(a config.Agent) AgentScreen {
	return AgentScreen{base: newBase("Agent " + a.ID), a: a}
}

func (s AgentScreen) Init() tea.Cmd { return nil }

func (s AgentScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
}

func (s AgentScreen) View() string {
	s.refresh()
	return s.render()
}

func (s AgentScreen) KeyBindings() []key.Binding { return s.bindings() }
func (s AgentScreen) Title() string              { return s.title }

func (s *AgentScreen) refresh() {
	perms := []string{
		"network: " + boolStr(s.a.Permissions.Network),
		"default sandbox: " + nonEmpty(s.a.Permissions.DefaultSandbox),
	}
	s.setContent(renderFields(s.theme, []field{
		{label: "id", value: s.a.ID},
		{label: "name", value: s.a.Name},
		{label: "roles", list: safeList(s.a.Roles)},
		{label: "skills", list: safeList(s.a.Skills)},
		{label: "context files", list: safeList(s.a.ContextFiles)},
		{label: "boot fragments", list: safeList(s.a.BootFragments)},
		{label: "permissions", list: perms},
	}))
}
