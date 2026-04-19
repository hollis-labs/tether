package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sahilm/fuzzy"

	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/layout"
)

// RowType is the catalog/runtime category a result row belongs to.
// Filter chips enable or disable visibility per category.
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
type Model struct {
	keys   KeyMap
	styles layout.Styles
	client *client.Client

	width  int
	height int
	search textinput.Model
	body   viewport.Model

	filters     map[RowType]bool
	rowsByType  map[RowType][]ResultRow
	visible     []ResultRow
	selectedIdx int

	loadRemaining int
	loadErrs      []error

	toasts toastQueue

	lastSearch string
}

// New constructs a Model. client may be nil (primarily for tests);
// when nil, no catalog data is loaded and the body stays on its
// placeholder message.
func New(c *client.Client) Model {
	ti := textinput.New()
	ti.Placeholder = "Search projects, agents, providers, launches, sessions…"
	ti.Prompt = "  "
	ti.Focus()

	vp := viewport.New(0, 0)
	vp.SetContent(placeholderBody())

	filters := make(map[RowType]bool, len(chipOrder))
	rowsByType := make(map[RowType][]ResultRow, len(chipOrder))
	for _, t := range chipOrder {
		filters[t] = true
		rowsByType[t] = nil
	}

	return Model{
		keys:          DefaultKeyMap(),
		styles:        layout.DefaultStyles(),
		client:        c,
		search:        ti,
		body:          vp,
		filters:       filters,
		rowsByType:    rowsByType,
		loadRemaining: len(chipOrder),
	}
}

func (m Model) Init() tea.Cmd {
	if m.client == nil {
		return textinput.Blink
	}
	return tea.Batch(textinput.Blink, loadAllCatalogCmd(m.client))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.refreshBody()
		return m, nil

	case catalogLoadedMsg:
		m.rowsByType[msg.typ] = msg.rows
		if msg.err != nil {
			m.loadErrs = append(m.loadErrs, msg.err)
		}
		if m.loadRemaining > 0 {
			m.loadRemaining--
		}
		m.recomputeVisible()
		m.refreshBody()
		return m, nil

	case launchResultMsg:
		if msg.err != nil {
			cmd := m.toasts.push(ToastError, "Launch failed: "+msg.err.Error())
			m.resize()
			m.refreshBody()
			return m, cmd
		}
		banner := fmt.Sprintf("Launched %s → session %s @ %s",
			msg.req.LaunchID, shortID(msg.res.SessionID), trimPath(msg.res.Workspace))
		cmd := m.toasts.push(ToastInfo, banner)
		m.resize()
		m.refreshBody()
		return m, cmd

	case toastExpiredMsg:
		m.toasts.remove(msg.ID)
		m.resize()
		m.refreshBody()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if msg.Type == tea.KeyEnter {
			return m.handleEnter()
		}
		if key.Matches(msg, m.keys.CycleChip) {
			m.cycleSoloChip(true)
			m.recomputeVisible()
			m.refreshBody()
			return m, nil
		}
		if key.Matches(msg, m.keys.CycleChipBack) {
			m.cycleSoloChip(false)
			m.recomputeVisible()
			m.refreshBody()
			return m, nil
		}
		if handled, next := m.handleChipToggle(msg); handled {
			next.recomputeVisible()
			next.refreshBody()
			return next, nil
		}
		if key.Matches(msg, m.keys.FocusSearch) && !m.search.Focused() {
			m.search.Focus()
			return m, textinput.Blink
		}
		if key.Matches(msg, m.keys.BlurSearch) && m.search.Focused() {
			m.search.Blur()
			return m, nil
		}

		// Selection navigation. Works regardless of focus so the user
		// can browse while typing the filter.
		if msg.Type == tea.KeyDown {
			m.moveSelection(1)
			m.refreshBody()
			return m, nil
		}
		if msg.Type == tea.KeyUp {
			m.moveSelection(-1)
			m.refreshBody()
			return m, nil
		}

		if !m.search.Focused() {
			if key.Matches(msg, m.keys.Quit) {
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.body, cmd = m.body.Update(msg)
			return m, cmd
		}

		if isViewportNavKey(msg) {
			var cmd tea.Cmd
			m.body, cmd = m.body.Update(msg)
			return m, cmd
		}
		prev := m.search.Value()
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		if m.search.Value() != prev {
			m.lastSearch = m.search.Value()
			m.recomputeVisible()
			m.refreshBody()
		}
		return m, cmd
	}

	return m, nil
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
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

// handleEnter launches the selected row if it's a LaunchRow; other
// row types emit an info toast in Sprint 1 (Sprint 2 binds Enter to
// "open detail view"). The toast keeps the interaction feeling
// responsive and tells the user what Enter will do.
func (m Model) handleEnter() (Model, tea.Cmd) {
	row := m.SelectedRow()
	if row == nil {
		return m, nil
	}
	lr, ok := row.(LaunchRow)
	if !ok {
		cmd := m.toasts.push(ToastInfo, fmt.Sprintf(
			"Enter launches [launches] rows; selected row is [%s] — detail views land in Sprint 2",
			row.Type()))
		m.resize()
		m.refreshBody()
		return m, cmd
	}
	if m.client == nil {
		return m, nil
	}
	return m, launchCmd(m.client, client.CreateAndLaunchRequest{LaunchID: lr.L.ID})
}

// cycleSoloChip advances the chip row through a rotating "solo"
// filter: all-on → projects-only → agents-only → providers-only →
// launches-only → sessions-only → all-on. Forward direction on Tab,
// reverse on Shift+Tab. Designed so a fresh state (all-on) cycles
// through each type and returns to all-on after len(chipOrder)+1
// presses.
func (m *Model) cycleSoloChip(forward bool) {
	current := m.currentSoloChip()
	// States, in order, that cycleSoloChip walks through:
	//   "" (all on) → chipOrder[0] → chipOrder[1] → ... → chipOrder[last] → ""
	states := make([]RowType, 0, len(chipOrder)+1)
	states = append(states, "") // all-on
	states = append(states, chipOrder...)

	idx := 0
	for i, s := range states {
		if s == current {
			idx = i
			break
		}
	}
	if forward {
		idx = (idx + 1) % len(states)
	} else {
		idx = (idx - 1 + len(states)) % len(states)
	}
	m.setSoloChip(states[idx])
}

// currentSoloChip returns the single enabled RowType when exactly one
// chip is on; returns "" when either all or a custom subset is enabled.
// This is what cycleSoloChip uses to decide the next state.
func (m Model) currentSoloChip() RowType {
	var only RowType
	enabled := 0
	for _, t := range chipOrder {
		if m.filters[t] {
			enabled++
			only = t
		}
	}
	if enabled == 1 {
		return only
	}
	return ""
}

// setSoloChip enables exactly `only` and disables the rest; passing ""
// enables all chips.
func (m *Model) setSoloChip(only RowType) {
	for _, t := range chipOrder {
		m.filters[t] = (only == "" || t == only)
	}
}

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
// size. Budget: bordered search 3 rows, chips 1, body border 2,
// footer 1 hint row + 1 row per active toast. Body takes what's left.
func (m *Model) resize() {
	base := 3 + 1 + 1 + 2 // search border + chips + footer hint + body border
	footerToasts := len(m.toasts.items())
	bodyHeight := m.height - base - footerToasts
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	bodyWidth := m.width - 2
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	m.body.Width = bodyWidth
	m.body.Height = bodyHeight
	m.search.Width = bodyWidth - 4
}

// recomputeVisible applies the current filter chips + search text to
// m.rowsByType, producing m.visible.
func (m *Model) recomputeVisible() {
	// Gather enabled rows in deterministic order (chipOrder).
	pool := make([]ResultRow, 0, 64)
	for _, t := range chipOrder {
		if !m.filters[t] {
			continue
		}
		pool = append(pool, m.rowsByType[t]...)
	}
	// Stable alpha-by-title baseline before fuzzy scoring.
	sort.SliceStable(pool, func(i, j int) bool {
		return pool[i].Title() < pool[j].Title()
	})

	query := strings.TrimSpace(m.search.Value())
	if query == "" {
		m.visible = pool
	} else {
		titles := make([]string, len(pool))
		for i, r := range pool {
			titles[i] = r.Title() + " " + r.ID()
		}
		matches := fuzzy.Find(query, titles)
		out := make([]ResultRow, 0, len(matches))
		for _, mm := range matches {
			out = append(out, pool[mm.Index])
		}
		m.visible = out
	}

	if m.selectedIdx >= len(m.visible) {
		m.selectedIdx = 0
	}
	if m.selectedIdx < 0 {
		m.selectedIdx = 0
	}
}

// moveSelection nudges the selected row index within the visible
// window, clamping at either end.
func (m *Model) moveSelection(delta int) {
	if len(m.visible) == 0 {
		return
	}
	next := m.selectedIdx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.visible) {
		next = len(m.visible) - 1
	}
	m.selectedIdx = next
}

// refreshBody re-renders the visible rows into the viewport. Also
// nudges the viewport's Y offset so the selected row stays visible.
func (m *Model) refreshBody() {
	if m.body.Width == 0 || m.body.Height == 0 {
		return
	}

	// Loading state: show a placeholder until first payload arrives.
	if m.loadRemaining == len(chipOrder) && len(m.visible) == 0 {
		m.body.SetContent(placeholderBody())
		return
	}

	if len(m.visible) == 0 {
		m.body.SetContent(emptyStateBody(m.loadErrs))
		return
	}

	var sb strings.Builder
	for i, r := range m.visible {
		sb.WriteString(renderRowLine(r, i == m.selectedIdx))
		sb.WriteByte('\n')
	}
	m.body.SetContent(sb.String())
	m.ensureSelectedVisible()
}

// ensureSelectedVisible scrolls the viewport so that the selected row
// sits inside the visible window. One row per rendered line.
func (m *Model) ensureSelectedVisible() {
	top := m.body.YOffset
	bottom := top + m.body.Height - 1
	switch {
	case m.selectedIdx < top:
		m.body.SetYOffset(m.selectedIdx)
	case m.selectedIdx > bottom:
		m.body.SetYOffset(m.selectedIdx - m.body.Height + 1)
	}
}

// SelectedRow returns the currently-highlighted row (or nil if the
// visible set is empty). Exposed for T-05 Enter-to-launch.
func (m Model) SelectedRow() ResultRow {
	if len(m.visible) == 0 {
		return nil
	}
	return m.visible[m.selectedIdx]
}

func renderRowLine(r ResultRow, selected bool) string {
	typeTag := fmt.Sprintf("[%s]", r.Type())
	line := fmt.Sprintf("%-11s %-30s  %s", typeTag, truncate(r.Title(), 30), r.Subtitle())
	if selected {
		return "▶ " + line
	}
	return "  " + line
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
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
	var block strings.Builder
	// Toasts render above the hint line so the user sees launch
	// feedback without hunting for it. Single-line styled text keeps
	// each toast to exactly one row of the vertical budget.
	for _, t := range m.toasts.items() {
		style := m.styles.ToastInfo
		glyph := "●"
		if t.Kind == ToastError {
			style = m.styles.ToastError
			glyph = "✗"
		}
		block.WriteString(style.Render(glyph + " " + t.Message))
		block.WriteByte('\n')
	}

	hints := []string{
		m.keyHint(m.keys.FocusSearch),
		m.keyHint(m.keys.BlurSearch),
		m.keyHint(m.keys.CycleChip),
		m.keyHint(m.keys.Quit),
		m.keyHint(m.keys.Help),
	}
	// Selection hint tells the user exactly which row they're pointing
	// at and what Enter will do. Empty when the pool is empty.
	selInfo := ""
	if sel := m.SelectedRow(); sel != nil {
		action := "detail (Sprint 2)"
		if _, ok := sel.(LaunchRow); ok {
			action = "launch"
		}
		selInfo = fmt.Sprintf("  ·  sel: [%s] %s — ⏎ %s", sel.Type(), sel.ID(), action)
	}
	status := ""
	if m.loadRemaining > 0 && m.client != nil {
		status = fmt.Sprintf("  ·  loading (%d/%d)…", len(chipOrder)-m.loadRemaining, len(chipOrder))
	} else if len(m.loadErrs) > 0 {
		status = fmt.Sprintf("  ·  %d load error(s)", len(m.loadErrs))
	} else if len(m.visible) > 0 {
		status = fmt.Sprintf("  ·  %d result(s)", len(m.visible))
	}
	block.WriteString(m.styles.Footer.Render(strings.Join(hints, "  ·  ") + status + selInfo))
	return m.styles.Frame.Render(block.String())
}

func (m Model) keyHint(b key.Binding) string {
	k, help := b.Help().Key, b.Help().Desc
	return m.styles.FooterKey.Render(k) + " " + help
}

func chipLabel(t RowType) string {
	return strings.ToUpper(string(t)[:1]) + string(t)[1:]
}

func placeholderBody() string {
	return "\n  Loading catalog…\n"
}

func emptyStateBody(errs []error) string {
	if len(errs) > 0 {
		var sb strings.Builder
		sb.WriteString("\n  No results — encountered load errors:\n")
		for _, e := range errs {
			sb.WriteString("    · " + e.Error() + "\n")
		}
		sb.WriteString("\n  Toggle filters (Alt+1..5) or clear the search.\n")
		return sb.String()
	}
	return "\n  No matches — clear the search or toggle a filter (Alt+1..5).\n"
}

func isViewportNavKey(msg tea.KeyMsg) bool {
	switch msg.Type { //nolint:exhaustive // intentional subset: scroll-only keys
	case tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		return true
	default:
		return false
	}
}
