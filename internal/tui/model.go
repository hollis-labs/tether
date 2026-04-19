package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/layout"
)

// RowType is the catalog/runtime category a result row belongs to.
// Filter chips enable or disable visibility per category. T-04 maps
// each List endpoint onto one of these; T-02 only needs the ordered
// set for the chip row.
type RowType string

const (
	RowTypeProjects  RowType = "projects"
	RowTypeAgents    RowType = "agents"
	RowTypeProviders RowType = "providers"
	RowTypeLaunches  RowType = "launches"
	RowTypeSessions  RowType = "sessions"
)

// chipOrder is the left-to-right rendering order of the chip row,
// matched 1:1 to Alt+1..5 bindings.
var chipOrder = []RowType{
	RowTypeProjects,
	RowTypeAgents,
	RowTypeProviders,
	RowTypeLaunches,
	RowTypeSessions,
}

// Model is the root Bubble Tea model for the mux TUI.
//
// T-02 adds the search input, chip filter state, viewport, and footer
// regions. T-04 replaces the placeholder body content with live catalog
// rows; T-05 wires Enter-to-launch on LaunchRow selection.
type Model struct {
	keys    KeyMap
	styles  layout.Styles
	width   int
	height  int
	search  textinput.Model
	body    viewport.Model
	filters map[RowType]bool
}

// New constructs a Model with scaffold defaults: all filters enabled,
// search input focused.
func New() Model {
	ti := textinput.New()
	ti.Placeholder = "Search projects, agents, providers, launches, sessions…"
	ti.Prompt = "  "
	ti.Focus()

	vp := viewport.New(0, 0)
	vp.SetContent(placeholderBody())

	filters := make(map[RowType]bool, len(chipOrder))
	for _, t := range chipOrder {
		filters[t] = true
	}

	return Model{
		keys:    DefaultKeyMap(),
		styles:  layout.DefaultStyles(),
		search:  ti,
		body:    vp,
		filters: filters,
	}
}

func (m Model) Init() tea.Cmd { return textinput.Blink }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case tea.KeyMsg:
		// Ctrl-C is the authoritative quit regardless of focus.
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}

		// Chip toggles always work; they use Alt-modifier so they
		// cannot collide with search-input typing.
		if handled, next := m.handleChipToggle(msg); handled {
			return next, nil
		}

		// Focus management for the search field.
		if key.Matches(msg, m.keys.FocusSearch) && !m.search.Focused() {
			m.search.Focus()
			return m, textinput.Blink
		}
		if key.Matches(msg, m.keys.BlurSearch) && m.search.Focused() {
			m.search.Blur()
			return m, nil
		}

		// When the search field is blurred, `q` quits and plain
		// movement keys (j/k/etc.) reach the viewport without the
		// textinput intercepting them.
		if !m.search.Focused() {
			if key.Matches(msg, m.keys.Quit) {
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.body, cmd = m.body.Update(msg)
			return m, cmd
		}

		// Search focused: route typing into textinput; let viewport
		// see navigation-only keys so users can still scroll while
		// typing.
		if isViewportNavKey(msg) {
			var cmd tea.Cmd
			m.body, cmd = m.body.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		// Pre-WindowSizeMsg: Bubble Tea will send one immediately.
		return ""
	}
	return layout.Render(
		m.renderSearch(),
		m.renderChips(),
		m.renderBody(),
		m.renderFooter(),
		m.width,
		m.height,
	)
}

// handleChipToggle flips the filter for a RowType when an Alt+N binding
// matches. Returns (true, updatedModel) when handled, (false, model)
// otherwise so the caller can fall through to other bindings.
func (m Model) handleChipToggle(msg tea.KeyMsg) (bool, Model) {
	bindings := []struct {
		b key.Binding
		t RowType
	}{
		{m.keys.ToggleProjects, RowTypeProjects},
		{m.keys.ToggleAgents, RowTypeAgents},
		{m.keys.ToggleProviders, RowTypeProviders},
		{m.keys.ToggleLaunches, RowTypeLaunches},
		{m.keys.ToggleSessions, RowTypeSessions},
	}
	for _, pair := range bindings {
		if key.Matches(msg, pair.b) {
			m.filters[pair.t] = !m.filters[pair.t]
			return true, m
		}
	}
	return false, m
}

// resize recomputes sub-model dimensions given the current terminal
// size. Layout budget: search occupies 3 rows (bordered), chip row 1,
// footer 1, body takes the rest; outer frame adds 2 rows of padding.
func (m *Model) resize() {
	const (
		searchRows = 3 // bordered input
		chipRows   = 1
		footerRows = 1
		// Body is bordered (+2 rows) but gets what's left.
		verticalOverhead = searchRows + chipRows + footerRows + 2
	)
	bodyHeight := m.height - verticalOverhead
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	bodyWidth := m.width - 2 // frame padding
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	m.body.Width = bodyWidth
	m.body.Height = bodyHeight
	m.search.Width = bodyWidth - 4 // account for prompt + border
}

func (m Model) renderSearch() string {
	return m.styles.Search.Width(m.width - 2).Render(m.search.View())
}

func (m Model) renderChips() string {
	out := make([]string, 0, len(chipOrder))
	for i, t := range chipOrder {
		label := fmt.Sprintf("%s (⌥%d)", chipLabel(t), i+1)
		if m.filters[t] {
			out = append(out, m.styles.ChipOn.Render(label))
		} else {
			out = append(out, m.styles.ChipOff.Render(label))
		}
	}
	return m.styles.Frame.Render(strings.Join(out, ""))
}

func (m Model) renderBody() string {
	return m.styles.Body.Width(m.width - 2).Render(m.body.View())
}

func (m Model) renderFooter() string {
	hints := []string{
		m.keyHint(m.keys.FocusSearch),
		m.keyHint(m.keys.BlurSearch),
		m.keyHint(m.keys.Quit),
		m.keyHint(m.keys.Help),
	}
	return m.styles.Frame.Render(m.styles.Footer.Render(strings.Join(hints, "  ·  ")))
}

func (m Model) keyHint(b key.Binding) string {
	k, help := b.Help().Key, b.Help().Desc
	return m.styles.FooterKey.Render(k) + " " + help
}

func chipLabel(t RowType) string {
	return strings.ToUpper(string(t)[:1]) + string(t)[1:]
}

func placeholderBody() string {
	return "\n  No catalog rows loaded yet.\n" +
		"  T-03 wires the daemon client; T-04 populates this viewport.\n"
}

// isViewportNavKey returns true for keys that should always scroll the
// body viewport, even when the search input is focused.
func isViewportNavKey(msg tea.KeyMsg) bool {
	switch msg.Type { //nolint:exhaustive // intentional subset: scroll-only keys
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		return true
	default:
		return false
	}
}
