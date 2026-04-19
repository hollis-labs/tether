package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// Model is the root Bubble Tea model for the mux TUI. T-01 scope is
// scaffold-only: enough state to track window size and react to the
// global quit key. T-02 extends it with search / chips / results /
// footer sub-models; T-03+ wires API data.
type Model struct {
	keys   KeyMap
	width  int
	height int
}

// New constructs a Model with scaffold defaults.
func New() Model {
	return Model{keys: DefaultKeyMap()}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if key.Matches(msg, m.keys.Quit) {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m Model) View() string {
	// Scaffold view. T-02 replaces this with the four-region layout
	// (search / chips / results / footer).
	return "Agent Mux TUI — scaffold. Press q or Ctrl-C to quit."
}
