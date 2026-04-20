// Package modal hosts reusable overlay Screens. Currently only
// ConfirmModal lives here; Sprint 3's delete flows will reuse it for
// catalog-object confirmations.
package modal

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ConfirmModal is a reusable yes/no modal. `y` or `Y` runs OnYes and
// pops; `n`, `N`, or Esc just pops. OnYes is a caller-supplied cmd
// factory — it runs AFTER the pop so the stack is already cleaned up
// by the time the side-effect-producing cmd executes.
type ConfirmModal struct {
	theme   theme.Theme
	title   string
	prompt  string
	onYes   func() tea.Cmd
	width   int
	height  int
	keyYes  key.Binding
	keyNo   key.Binding
	keyBack key.Binding
}

func NewConfirm(title, prompt string, onYes func() tea.Cmd) *ConfirmModal {
	return &ConfirmModal{
		theme:   theme.Default(),
		title:   title,
		prompt:  prompt,
		onYes:   onYes,
		keyYes:  key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "yes")),
		keyNo:   key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("n", "no")),
		keyBack: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

func (m *ConfirmModal) Init() tea.Cmd { return nil }

func (m *ConfirmModal) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if key.Matches(msg, m.keyYes) {
			yes := m.onYes
			if yes == nil {
				return m, screen.Pop()
			}
			return m, tea.Batch(screen.Pop(), yes())
		}
		if key.Matches(msg, m.keyNo) || key.Matches(msg, m.keyBack) {
			return m, screen.Pop()
		}
	}
	return m, nil
}

func (m *ConfirmModal) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	panelWidth := 50
	if panelWidth > m.width-4 {
		panelWidth = m.width - 4
	}

	title := m.theme.Header().Width(panelWidth - 2).Render(m.title)
	prompt := m.theme.FieldValue().Render(m.prompt)
	hints := []string{
		m.theme.FooterKey().Render("y") + " yes",
		m.theme.FooterKey().Render("n") + " no",
		m.theme.FooterKey().Render("esc") + " cancel",
	}
	footer := m.theme.Footer().Render(strings.Join(hints, "  ·  "))

	panel := lipgloss.JoinVertical(lipgloss.Left, title, "", prompt, "", footer)
	boxed := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Accent()).
		Padding(0, 2).
		Render(panel)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, boxed)
}

func (m *ConfirmModal) KeyBindings() []key.Binding {
	return []key.Binding{m.keyYes, m.keyNo, m.keyBack}
}

func (m *ConfirmModal) Title() string { return m.title }
