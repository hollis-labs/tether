package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// Model is the Bubble Tea root for the mux TUI. It's a thin wrapper
// around a screen.Stack: every visible pane is a Screen, and Model
// delegates Init / Update / View to whatever screen is on top. The
// only global state Model owns is the terminal size and the stack
// itself — everything else lives on the screens.
type Model struct {
	stack  *screen.Stack
	width  int
	height int
}

// New constructs the root Model with MainScreen as the initial top-of-
// stack screen. client may be nil for tests.
func New(c *client.Client) Model {
	return Model{stack: screen.NewStack(NewMainScreen(c))}
}

func (m Model) Init() tea.Cmd {
	return m.stack.Top().Init()
}

// Update routes messages to the top screen, with three exceptions:
//
//   - PushScreenMsg / PopScreenMsg mutate the stack and re-send the
//     current WindowSizeMsg to the new top so it lays out at the
//     correct dimensions immediately.
//   - WindowSizeMsg is captured in Model (so pushes/pops can re-send
//     it) and also forwarded to the top screen.
//
// Crucially, Model does NOT intercept Ctrl-C. The screen-stack design
// puts key handling under the top screen's control — MainScreen maps
// Ctrl-C to tea.Quit, AttachScreen (T-04) will map it to sending a
// SIGINT byte to the attached session. A root-level Ctrl-C would
// preempt that.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case screen.PushScreenMsg:
		m.stack.Push(msg.Screen)
		initCmd := msg.Screen.Init()
		if m.width > 0 && m.height > 0 {
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack.Replace(sized)
			return m, tea.Batch(initCmd, sizeCmd)
		}
		return m, initCmd

	case screen.PopScreenMsg:
		if m.stack.Pop() == nil {
			// Refused — already at the main screen; nothing to do.
			return m, nil
		}
		if m.width > 0 && m.height > 0 {
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack.Replace(sized)
			return m, sizeCmd
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Fall through to delegate: the top screen needs the size too.
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd

	default:
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd
	}
}

func (m Model) View() string {
	return m.stack.Top().View()
}
