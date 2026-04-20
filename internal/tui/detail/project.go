package detail

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/bubbles/key"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// ProjectScreen renders a config.Project.
type ProjectScreen struct {
	base
	p config.Project
}

func NewProjectScreen(p config.Project) ProjectScreen {
	s := ProjectScreen{base: newBase("Project " + p.ID), p: p}
	return s
}

func (s ProjectScreen) Init() tea.Cmd { return nil }

func (s ProjectScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
}

func (s ProjectScreen) View() string {
	s.refresh()
	return s.render()
}

func (s ProjectScreen) KeyBindings() []key.Binding { return s.bindings() }
func (s ProjectScreen) Title() string              { return s.title }

func (s *ProjectScreen) refresh() {
	s.setContent(renderFields(s.theme, []field{
		{label: "id", value: s.p.ID},
		{label: "name", value: s.p.Name},
		{label: "repo root", value: s.p.RepoRoot},
		{label: "tracking root", value: s.p.TrackingRoot},
		{label: "workspace mode", value: s.p.Workspace.DefaultMode},
		{label: "worktree base", value: s.p.Workspace.WorktreeBase},
		{label: "session root", value: s.p.Workspace.SessionRoot},
		{label: "knowledge base", list: safeList(s.p.KnowledgeBase)},
		{label: "boot fragments", list: safeList(s.p.BootFragments)},
	}))
}

func safeList(xs []string) []string {
	if len(xs) == 0 {
		return []string{"—"}
	}
	return xs
}
