package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/palette"
	"github.com/chrispian/agent-mux/internal/tui/panel"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// proxyEventReceiver is implemented by screens that want to receive the
// root model's polled proxy event snapshot directly instead of polling
// on their own. ToolCallFeedScreen implements this.
type proxyEventReceiver interface {
	screen.Screen
	ReceiveProxyEvents(evts []api.ProxyEventDTO) (screen.Screen, tea.Cmd)
}

const proxyEventPollInterval = 500 * time.Millisecond
const proxyEventLimit = 200

// proxyEventTickMsg triggers a background poll of proxy events.
type proxyEventTickMsg struct{}

// proxyEventsLoadedMsg carries a fresh snapshot of proxy events.
type proxyEventsLoadedMsg struct {
	events []api.ProxyEventDTO
}

func proxyEventTickCmd() tea.Cmd {
	return tea.Tick(proxyEventPollInterval, func(time.Time) tea.Msg {
		return proxyEventTickMsg{}
	})
}

func proxyEventPollCmd(c *client.Client) tea.Cmd {
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		evs, _ := c.QueryProxyEvents(context.Background(), proxyEventLimit)
		return proxyEventsLoadedMsg{events: evs}
	}
}

// globalKeys holds the root-level key bindings that are handled by Model
// before any message reaches the screen stack or the panel.
type globalKeys struct {
	PanelToggle key.Binding // Ctrl+\ — show/hide panel
	PanelFocus  key.Binding // Ctrl+→ or F3 — focus panel
	AcceptAll   key.Binding // A — accept all queued prompts
}

func defaultGlobalKeys() globalKeys {
	return globalKeys{
		PanelToggle: key.NewBinding(key.WithKeys("ctrl+\\"), key.WithHelp("ctrl+\\", "panel")),
		PanelFocus:  key.NewBinding(key.WithKeys("ctrl+right", "f3"), key.WithHelp("ctrl+→", "focus panel")),
		AcceptAll:   key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "accept all")),
	}
}

// Model is the Bubble Tea root for the mux TUI. It owns the screen stack,
// the command palette registry, and the side panel. The panel is a
// root-level sibling of the stack — not pushed onto it — so it persists
// across screen transitions and can be toggled from anywhere.
type Model struct {
	stack      *screen.Stack
	registry   *palette.Registry
	sidePanel  panel.Model
	globalKeys globalKeys
	width      int
	height     int

	// tuiClient is kept here so the root model can poll proxy events
	// independently of which screen is on top of the stack.
	tuiClient *client.Client

	// proxyEvents is the latest snapshot of proxy tool call events,
	// polled globally every 500ms. The feed screen reads from this
	// slice rather than issuing its own polling commands.
	proxyEvents []api.ProxyEventDTO
}

// New constructs the root Model with MainScreen as the initial top-of-
// stack screen and registers the default Sprint-2 verbs with the
// palette registry. client may be nil for tests.
func New(c *client.Client) Model {
	return NewWithOptions(c, Options{})
}

// NewWithOptions constructs the root Model with the given options.
func NewWithOptions(c *client.Client, opts Options) Model {
	reg := palette.NewRegistry()
	registerDefaultVerbs(reg)
	main := NewMainScreen(c)
	return Model{
		stack:      screen.NewStack(main),
		registry:   reg,
		sidePanel:  panel.New(),
		globalKeys: defaultGlobalKeys(),
		tuiClient:  c,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.stack.Top().Init(),
		proxyEventTickCmd(), // start global proxy event poll immediately
	)
}

// Update routes messages to the top screen, with the following exceptions:
//
//   - openPaletteMsg / PushScreenMsg / PopScreenMsg mutate the stack and
//     re-send the current WindowSizeMsg to the new top so it lays out at
//     the correct dimensions immediately.
//   - Panel messages (PanelPushMsg, PanelPinMsg, etc.) are routed to the
//     side panel. PanelResponseMsg is forwarded to the top screen.
//   - WindowSizeMsg is captured in Model (so pushes/pops can re-send it);
//     the panel and the top screen both receive a size notification, but
//     the stack gets only its share of the horizontal space.
//   - tea.KeyMsg is intercepted for global panel shortcuts first, then
//     routed to the panel (when focused) or the stack.
//
// Crucially, Model does NOT intercept Ctrl-C. The screen-stack design
// puts key handling under the top screen's control — MainScreen maps
// Ctrl-C to tea.Quit, AttachScreen (T-04) will map it to sending a
// SIGINT byte to the attached session. A root-level Ctrl-C would
// preempt that.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case openPaletteMsg:
		overlay := palette.NewOverlay(m.registry)
		return m.Update(screen.PushScreenMsg{Screen: overlay})

	case screen.PushScreenMsg:
		m.stack.Push(msg.Screen)
		initCmd := msg.Screen.Init()
		// If the pushed screen is a feed screen, immediately seed it with
		// the current proxy event snapshot so it renders without waiting.
		if feed, ok := m.stack.Top().(proxyEventReceiver); ok && len(m.proxyEvents) > 0 {
			seeded, seedCmd := feed.ReceiveProxyEvents(m.proxyEvents)
			m.stack.Replace(seeded)
			initCmd = tea.Batch(initCmd, seedCmd)
		}
		if m.width > 0 && m.height > 0 {
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height})
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
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height})
			m.stack.Replace(sized)
			return m, sizeCmd
		}
		return m, nil

	case panel.PanelPushMsg, panel.PanelPinMsg, panel.PanelToggleOpenMsg,
		panel.PanelToggleSlotMsg, panel.PanelTogglePinMsg,
		panel.PanelAcceptAllMsg, panel.PanelFocusMsg, panel.PanelDismissMsg:
		wasOpen := m.sidePanel.IsOpen()
		newPanel, cmd := m.sidePanel.Update(msg)
		m.sidePanel = newPanel.(panel.Model)
		m.sidePanel.SetSize(m.panelWidth(), m.height)
		if m.sidePanel.IsOpen() != wasOpen && m.width > 0 {
			top := m.stack.Top()
			newTop, sizeCmd := top.Update(tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height})
			m.stack.Replace(newTop)
			return m, tea.Batch(cmd, sizeCmd)
		}
		return m, cmd

	case panel.PanelResponseMsg:
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sidePanel.SetSize(m.panelWidth(), m.height)
		stackMsg := tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height}
		top := m.stack.Top()
		newTop, cmd := top.Update(stackMsg)
		m.stack.Replace(newTop)
		return m, cmd

	case proxyEventTickMsg:
		// Fire an async poll regardless of which screen is on top.
		return m, proxyEventPollCmd(m.tuiClient)

	case proxyEventsLoadedMsg:
		if msg.events != nil {
			m.proxyEvents = msg.events
		}
		// Forward the latest snapshot to the feed screen if it's currently on top.
		top := m.stack.Top()
		if feed, ok := top.(proxyEventReceiver); ok {
			newTop, cmd := feed.ReceiveProxyEvents(m.proxyEvents)
			m.stack.Replace(newTop)
			return m, tea.Batch(proxyEventTickCmd(), cmd)
		}
		return m, proxyEventTickCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd
	}
}

// handleKey handles key messages at the root level, intercepting global
// panel shortcuts before routing to the panel or the screen stack.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+\ — toggle panel open/closed from anywhere
	if key.Matches(msg, m.globalKeys.PanelToggle) {
		newPanel, cmd := m.sidePanel.Update(panel.PanelToggleOpenMsg{})
		m.sidePanel = newPanel.(panel.Model)
		m.sidePanel.SetSize(m.panelWidth(), m.height)
		if m.width > 0 {
			top := m.stack.Top()
			newTop, sizeCmd := top.Update(tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height})
			m.stack.Replace(newTop)
			return m, tea.Batch(cmd, sizeCmd)
		}
		return m, cmd
	}
	// Ctrl+→ / F3 — focus panel when open
	if key.Matches(msg, m.globalKeys.PanelFocus) && m.sidePanel.IsOpen() {
		newPanel, cmd := m.sidePanel.Update(panel.PanelFocusMsg{})
		m.sidePanel = newPanel.(panel.Model)
		return m, cmd
	}
	// A — accept all queued prompts
	if key.Matches(msg, m.globalKeys.AcceptAll) && m.sidePanel.HasEphemeral() {
		wasOpen := m.sidePanel.IsOpen()
		newPanel, cmd := m.sidePanel.Update(panel.PanelAcceptAllMsg{})
		m.sidePanel = newPanel.(panel.Model)
		if wasOpen && !m.sidePanel.IsOpen() && m.width > 0 {
			top := m.stack.Top()
			newTop, sizeCmd := top.Update(tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height})
			m.stack.Replace(newTop)
			return m, tea.Batch(cmd, sizeCmd)
		}
		return m, cmd
	}
	// Route to panel when focused; otherwise to screen stack
	if m.sidePanel.IsFocused() {
		newPanel, cmd := m.sidePanel.Update(msg)
		m.sidePanel = newPanel.(panel.Model)
		return m, cmd
	}
	top := m.stack.Top()
	newTop, cmd := top.Update(msg)
	m.stack.Replace(newTop)
	return m, cmd
}

func (m Model) View() string {
	stackView := m.stack.Top().View()
	panelView := m.sidePanel.View()
	if panelView == "" {
		return stackView
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, stackView, panelView)
}

// stackWidth returns the width for the screen stack.
// When panel is closed or terminal too narrow, stack takes full width.
func (m Model) stackWidth() int {
	pw := m.panelWidth()
	if pw == 0 {
		return m.width
	}
	return m.width - pw
}

// panelWidth returns the width allocated to the side panel.
// Returns 0 if panel is closed or terminal too narrow for a useful panel.
func (m Model) panelWidth() int {
	if !m.sidePanel.IsOpen() {
		return 0
	}
	pw := m.width * 30 / 100
	if pw < 40 {
		return 0 // terminal too narrow; suppress panel rather than squash the stack
	}
	return pw
}
