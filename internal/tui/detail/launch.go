package detail

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// LaunchScreen renders a config.Launch.
type LaunchScreen struct {
	base
	l config.Launch
}

func NewLaunchScreen(l config.Launch) LaunchScreen {
	return LaunchScreen{base: newBase("Launch " + l.ID), l: l}
}

func (s LaunchScreen) Init() tea.Cmd { return nil }

func (s LaunchScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
}

func (s LaunchScreen) View() string {
	s.refresh()
	return s.render()
}

func (s LaunchScreen) KeyBindings() []key.Binding { return s.bindings() }
func (s LaunchScreen) Title() string              { return s.title }

func (s *LaunchScreen) refresh() {
	s.setContent(renderFields(s.theme, []field{
		{label: "id", value: s.l.ID},
		{label: "project", value: s.l.Project},
		{label: "agent", value: s.l.Agent},
		{label: "provider", value: s.l.Provider},
		{label: "workspace mode", value: s.l.Workspace.Mode},
		{label: "worktree name", value: s.l.Workspace.WorktreeName},
		{label: "write home", value: s.l.Workspace.WriteHome},
		{label: "include project boot", value: boolStr(s.l.Prompt.IncludeProjectBoot)},
		{label: "include agent boot", value: boolStr(s.l.Prompt.IncludeAgentBoot)},
		{label: "include knowledge base", value: boolStr(s.l.Prompt.IncludeKnowledgeBase)},
		{label: "env overrides", list: pairsOf(s.l.Overrides.Env)},
	}))
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
