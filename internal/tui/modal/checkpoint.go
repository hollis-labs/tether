package modal

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// CheckpointSubmitMsg carries the form values back to the caller after
// the user confirms the checkpoint form.
type CheckpointSubmitMsg struct {
	Status  string
	Summary string
}

// statuses is the ordered list of valid checkpoint status values.
var statuses = []string{"active", "paused", "completed", "escalated"}

// CheckpointModal is a two-field form for creating a checkpoint:
// - Status: cycle through active/paused/completed/escalated with Tab
// - Summary: free-text single-line input
//
// Enter submits; Esc/q cancels.
type CheckpointModal struct {
	theme         theme.Theme
	width         int
	height        int
	statusIdx     int
	summary       textinput.Model
	keySubmit     key.Binding
	keyCancel     key.Binding
	keyNextStatus key.Binding
}

func NewCheckpointModal() *CheckpointModal {
	ti := textinput.New()
	ti.Placeholder = "Brief summary of current state…"
	ti.Focus()
	ti.Width = 50
	return &CheckpointModal{
		theme:         theme.Default(),
		summary:       ti,
		keySubmit:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "save")),
		keyCancel:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		keyNextStatus: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "cycle status")),
	}
}

func (m *CheckpointModal) Init() tea.Cmd {
	return textinput.Blink
}

func (m *CheckpointModal) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if key.Matches(msg, m.keyCancel) {
			return m, screen.Pop()
		}
		if key.Matches(msg, m.keyNextStatus) {
			m.statusIdx = (m.statusIdx + 1) % len(statuses)
			return m, nil
		}
		if key.Matches(msg, m.keySubmit) {
			submit := CheckpointSubmitMsg{
				Status:  statuses[m.statusIdx],
				Summary: strings.TrimSpace(m.summary.Value()),
			}
			return m, tea.Batch(screen.Pop(), func() tea.Msg { return submit })
		}
	}

	var cmd tea.Cmd
	m.summary, cmd = m.summary.Update(msg)
	return m, cmd
}

func (m *CheckpointModal) View() string {
	t := m.theme
	title := t.Header().Render("Checkpoint")

	statusLine := "Status:  "
	for i, s := range statuses {
		if i == m.statusIdx {
			statusLine += lipgloss.NewStyle().
				Bold(true).
				Foreground(t.Accent()).
				Render("[" + s + "]")
		} else {
			statusLine += " " + s
		}
		if i < len(statuses)-1 {
			statusLine += " "
		}
	}

	helpText := t.FooterKey().Render("tab") + " cycle  " +
		t.FooterKey().Render("enter") + " save  " +
		t.FooterKey().Render("esc") + " cancel"

	body := lipgloss.JoinVertical(lipgloss.Left,
		statusLine,
		"",
		"Summary:",
		m.summary.View(),
		"",
		helpText,
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent()).
		Padding(1, 2).
		Width(60).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, "", body))

	if m.width == 0 || m.height == 0 {
		return box
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m *CheckpointModal) KeyBindings() []key.Binding {
	return []key.Binding{m.keyNextStatus, m.keySubmit, m.keyCancel}
}

func (m *CheckpointModal) Title() string { return "Checkpoint" }
