package detail

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// ProviderScreen renders a config.Provider.
type ProviderScreen struct {
	base
	p config.Provider
}

func NewProviderScreen(p config.Provider) ProviderScreen {
	return ProviderScreen{base: newBase("Provider " + p.ID), p: p}
}

func (s ProviderScreen) Init() tea.Cmd { return nil }

func (s ProviderScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
}

func (s ProviderScreen) View() string {
	s.refresh()
	return s.render()
}

func (s ProviderScreen) KeyBindings() []key.Binding { return s.bindings() }
func (s ProviderScreen) Title() string              { return s.title }

func (s *ProviderScreen) refresh() {
	s.setContent(renderFields(s.theme, []field{
		{label: "id", value: s.p.ID},
		{label: "type", value: s.p.Type},
		{label: "command", value: s.p.Command},
		{label: "args", list: safeList(s.p.Args)},
		{label: "bootstrap mode", value: s.p.Bootstrap.Mode},
		{label: "bootstrap prompt prefix", value: s.p.Bootstrap.PromptPrefix},
		{label: "env mode", value: s.p.Env.Mode},
		{label: "env passthrough", list: safeList(s.p.Env.Passthrough)},
		{label: "env redact", list: safeList(s.p.Env.Redact)},
	}))
}

// pairsOf flattens a string map to "k=v" entries for display.
func pairsOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	if len(out) == 0 {
		return []string{"—"}
	}
	return out
}
