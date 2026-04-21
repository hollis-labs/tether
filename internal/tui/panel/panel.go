package panel

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// PanelPushMsg asks the panel to enqueue a new ephemeral Content.
type PanelPushMsg struct{ Content Content }

// PanelPinMsg asks the panel to load a pinned file into the persistent slot.
type PanelPinMsg struct {
	Path string
	Body string
}

// PanelToggleOpenMsg toggles the panel open/closed.
type PanelToggleOpenMsg struct{}

// PanelToggleSlotMsg switches the active slot when both slots have content.
type PanelToggleSlotMsg struct{}

// PanelTogglePinMsg toggles the pinned flag (keep-open after ephemeral clears).
type PanelTogglePinMsg struct{}

// PanelAcceptAllMsg accepts all queued ephemeral items using their default values.
type PanelAcceptAllMsg struct{}

// PanelFocusMsg gives keyboard focus to the panel.
type PanelFocusMsg struct{}

// PanelDismissMsg dismisses the front ephemeral item.
type PanelDismissMsg struct{}

// PanelResponseMsg carries the confirmed response back to the root model.
type PanelResponseMsg struct {
	ToolUseID string
	Value     string
}

// ---------------------------------------------------------------------------
// Key bindings
// ---------------------------------------------------------------------------

// panelKeys are the bindings handled by the panel model itself.
// Navigation (↑↓/jk) is intentionally absent — each Content type owns
// its own navigation bindings so they can vary by content kind.
type panelKeys struct {
	Confirm, ToggleSlot, TogglePin, Refresh, Hints, BlurPanel key.Binding
}

func defaultPanelKeys() panelKeys {
	return panelKeys{
		Confirm:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		ToggleSlot: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch slot")),
		TogglePin:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pin/unpin")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Hints:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "hints")),
		BlurPanel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "blur")),
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// Model is the side-panel Bubble Tea model.
type Model struct {
	open       bool
	pinned     bool
	active     Slot
	queue      []Content // pending ephemeral prompts; front is active
	persistent Content   // user-pinned; nil when empty
	pinnedPath string    // path of the pinned file
	focused    bool
	width      int
	height     int
	th         theme.Theme
	keys       panelKeys
	showHints  bool
}

// New returns a closed, empty panel with default key bindings.
func New() Model {
	return Model{
		th:   theme.Default(),
		keys: defaultPanelKeys(),
	}
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

func (m Model) IsOpen() bool        { return m.open }
func (m Model) IsFocused() bool     { return m.focused }
func (m Model) HasEphemeral() bool  { return len(m.queue) > 0 }
func (m Model) HasPersistent() bool { return m.persistent != nil }
func (m Model) Width() int          { return m.width }

// ActiveContent returns the content in the currently active slot, or nil.
func (m Model) ActiveContent() Content {
	switch m.active {
	case SlotEphemeral:
		if len(m.queue) > 0 {
			return m.queue[0]
		}
	case SlotPersistent:
		if m.persistent != nil {
			return m.persistent
		}
	}
	return nil
}

// SetSize updates the panel dimensions.
func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// ---------------------------------------------------------------------------
// Init / Update / View
// ---------------------------------------------------------------------------

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PanelPushMsg:
		m.queue = append(m.queue, msg.Content)
		m.open = true
		m.active = SlotEphemeral
		return m, nil

	case PanelPinMsg:
		m.persistent = NewViewportContent(msg.Body)
		m.pinnedPath = msg.Path
		m.open = true
		if len(m.queue) == 0 {
			m.active = SlotPersistent
		}
		return m, nil

	case PanelToggleOpenMsg:
		m.open = !m.open
		if !m.open {
			m.focused = false
		}
		return m, nil

	case PanelToggleSlotMsg:
		if len(m.queue) > 0 && m.persistent != nil {
			if m.active == SlotEphemeral {
				m.active = SlotPersistent
			} else {
				m.active = SlotEphemeral
			}
		}
		return m, nil

	case PanelTogglePinMsg:
		m.pinned = !m.pinned
		return m, nil

	case PanelFocusMsg:
		if m.open {
			m.focused = true
		}
		return m, nil

	case PanelDismissMsg:
		return m.dismissFront()

	case PanelAcceptAllMsg:
		return m.acceptAll()

	case tea.KeyMsg:
		if !m.focused {
			return m, nil
		}
		return m.handleKey(msg)

	default:
		return m.forwardToContent(msg)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (m Model) dismissFront() (tea.Model, tea.Cmd) {
	if len(m.queue) > 0 {
		m.queue = m.queue[1:]
	}
	if len(m.queue) == 0 {
		if !m.pinned {
			m.open = false
			m.focused = false
		} else if m.persistent != nil {
			m.active = SlotPersistent
		}
	}
	return m, nil
}

func (m Model) acceptAll() (tea.Model, tea.Cmd) {
	if len(m.queue) == 0 {
		return m, nil
	}
	cmds := make([]tea.Cmd, 0, len(m.queue))
	for _, item := range m.queue {
		// Skip display-only viewport content — no response to send.
		if item.Kind() == KindViewport {
			continue
		}
		cmds = append(cmds, panelResponseCmd(item.ToolUseID(), item.DefaultValue()))
	}
	m.queue = nil
	if !m.pinned {
		m.open = false
		m.focused = false
	} else if m.persistent != nil {
		m.active = SlotPersistent
	}
	return m, tea.Batch(cmds...)
}

func panelResponseCmd(toolUseID, value string) tea.Cmd {
	return func() tea.Msg {
		return PanelResponseMsg{ToolUseID: toolUseID, Value: value}
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.BlurPanel):
		m.focused = false
		return m, nil

	case key.Matches(msg, m.keys.TogglePin):
		m.pinned = !m.pinned
		return m, nil

	case key.Matches(msg, m.keys.ToggleSlot):
		if len(m.queue) > 0 && m.persistent != nil {
			if m.active == SlotEphemeral {
				m.active = SlotPersistent
			} else {
				m.active = SlotEphemeral
			}
		}
		return m, nil

	case key.Matches(msg, m.keys.Refresh):
		// file re-read is root's responsibility; no-op here
		return m, nil

	case key.Matches(msg, m.keys.Hints):
		m.showHints = !m.showHints
		return m, nil

	case key.Matches(msg, m.keys.Confirm):
		return m.confirmActive()

	default:
		return m.forwardToContent(msg)
	}
}

func (m Model) confirmActive() (tea.Model, tea.Cmd) {
	if m.active != SlotEphemeral || len(m.queue) == 0 {
		return m, nil
	}
	front := m.queue[0]
	// Viewport is display-only — Enter dismisses without sending a response.
	// Interactive types (YesNo, MultiChoice, TextInput) always send a response,
	// even for text-sentinel prompts that carry no tool_use ID.
	if front.Kind() == KindViewport {
		return m.dismissFront()
	}
	var value string
	switch c := front.(type) {
	case *YesNoContent:
		value = c.SelectedValue()
	case *MultiChoiceContent:
		value = c.SelectedValue()
	case *TextInputContent:
		value = c.CurrentValue()
	default:
		value = front.DefaultValue()
	}
	resp := panelResponseCmd(front.ToolUseID(), value)
	newM, _ := m.dismissFront()
	return newM, resp
}

func (m Model) forwardToContent(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.active {
	case SlotEphemeral:
		if len(m.queue) > 0 {
			updated, c := m.queue[0].Update(msg)
			m.queue[0] = updated
			cmd = c
		}
	case SlotPersistent:
		if m.persistent != nil {
			updated, c := m.persistent.Update(msg)
			m.persistent = updated
			cmd = c
		}
	}
	return m, cmd
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m Model) View() string {
	if !m.open || m.width < 40 {
		return ""
	}

	borderColor := m.th.Border()
	if m.focused {
		borderColor = m.th.Accent()
	}

	// Build header label.
	var label string
	switch m.active {
	case SlotEphemeral:
		qlen := len(m.queue)
		if qlen > 1 {
			label = fmt.Sprintf("prompt (%d)", qlen)
		} else {
			label = "prompt"
		}
	case SlotPersistent:
		if m.pinnedPath != "" {
			label = m.pinnedPath
		} else {
			label = "pinned"
		}
	}

	// Pin indicator.
	pinIndicator := ""
	if m.pinned {
		pinIndicator = " 📌"
	}

	// Key hints.
	hints := ""
	if m.showHints {
		hints = "  [esc] blur  [p] pin  [tab] slot  [?] hints"
	} else if m.focused {
		hints = "  [?] hints"
	}

	headerText := m.th.Header().Render(label+pinIndicator) + m.th.Footer().Render(hints)

	headerStyle := lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(borderColor)

	header := headerStyle.Render(headerText)

	// Body.
	bodyHeight := m.height - 2
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	var body string
	if c := m.ActiveContent(); c != nil {
		body = c.View(m.width, bodyHeight)
	} else {
		body = m.th.Footer().Render("(empty)")
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}
