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

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/detail"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
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

// MainScreen is the results-list / launcher pane — the first screen
// the user sees when they open the TUI. It implements screen.Screen
// so the root Model can route to it through the stack introduced in
// Sprint 2.
//
// Most of the state that used to live on the Sprint 1 root Model now
// lives here: search input, filter chips, viewport, selection, load
// state, toasts. Toasts stay on MainScreen (rather than moving to the
// root) because every toast we emit today originates from a
// main-screen action; Sprint 2's attach/detail screens emit their own
// toasts on their own surfaces.
type MainScreen struct {
	keys   KeyMap
	theme  theme.Theme
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

// NewMainScreen constructs the main screen. client may be nil
// (primarily for tests); when nil, no catalog data is loaded and the
// body stays on its placeholder message.
func NewMainScreen(c *client.Client) MainScreen {
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

	return MainScreen{
		keys:          DefaultKeyMap(),
		theme:         theme.Default(),
		client:        c,
		search:        ti,
		body:          vp,
		filters:       filters,
		rowsByType:    rowsByType,
		loadRemaining: len(chipOrder),
	}
}

// Title implements screen.Screen. Used by the root's breadcrumb.
func (m MainScreen) Title() string { return "Main" }

// KeyBindings implements screen.Screen — returned in the order shown
// in the footer hint line.
func (m MainScreen) KeyBindings() []key.Binding {
	return []key.Binding{
		m.keys.OpenDetail,
		m.keys.FocusSearch,
		m.keys.BlurSearch,
		m.keys.CycleChip,
		m.keys.Quit,
		m.keys.Help,
	}
}

func (m MainScreen) Init() tea.Cmd {
	if m.client == nil {
		return textinput.Blink
	}
	return tea.Batch(textinput.Blink, loadAllCatalogCmd(m.client))
}

func (m MainScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
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
		// Success: banner + auto-attach + re-fetch sessions so the
		// new session appears in the main list when the user Esc's
		// back. Without the re-fetch, the list is frozen at the
		// startup snapshot and the newly-launched session never
		// shows up.
		banner := fmt.Sprintf("Launched %s → session %s — attaching…",
			msg.req.LaunchID, shortID(msg.res.SessionID))
		toastCmd := m.toasts.push(ToastInfo, banner)
		m.resize()
		m.refreshBody()
		sess := api.SessionDTO{
			ID:        msg.res.SessionID,
			Workspace: msg.res.Workspace,
			State:     "running",
			LaunchID:  msg.req.LaunchID,
		}
		attachPush := screen.Push(detail.NewAttachScreen(sess, m.client))
		refreshSessions := loadSessionsCmd(m.client)
		return m, tea.Batch(toastCmd, attachPush, refreshSessions)

	case toastExpiredMsg:
		m.toasts.remove(msg.ID)
		m.resize()
		m.refreshBody()
		return m, nil

	case screen.ToastEmitMsg:
		kind := ToastInfo
		if msg.Kind == screen.ToastError {
			kind = ToastError
		}
		cmd := m.toasts.push(kind, msg.Text)
		m.resize()
		m.refreshBody()
		return m, cmd

	case applyFilterMsg:
		m.setSoloChip(msg.solo)
		m.recomputeVisible()
		m.refreshBody()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if msg.Type == tea.KeyCtrlK || (msg.Type == tea.KeyRunes && string(msg.Runes) == ":") {
			return m, openPalette()
		}
		if msg.Type == tea.KeyEnter {
			return m.handleEnter()
		}
		if key.Matches(msg, m.keys.OpenDetail) {
			return m.openDetail()
		}
		if key.Matches(msg, m.keys.Refresh) {
			if m.client != nil {
				m.loadRemaining = len(chipOrder)
				m.loadErrs = nil
				return m, loadAllCatalogCmd(m.client)
			}
			return m, nil
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

func (m MainScreen) View() string {
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

// handleEnter dispatches on the selected row type so Enter always
// does "the main action" for whatever you're looking at:
//
//   - LaunchRow  → launch (+ auto-attach via launchResultMsg)
//   - SessionRow → attach to the running session
//   - everything else → open the detail screen (same as →)
func (m MainScreen) handleEnter() (MainScreen, tea.Cmd) {
	row := m.SelectedRow()
	if row == nil {
		return m, nil
	}
	if m.client == nil {
		// Can't launch or attach without a daemon client — fall
		// back to the detail screen so Enter still feels responsive
		// in test/offline mode.
		return m.openDetail()
	}
	switch r := row.(type) {
	case LaunchRow:
		return m, launchCmd(m.client, client.CreateAndLaunchRequest{LaunchID: r.L.ID})
	case SessionRow:
		return m, screen.Push(detail.NewAttachScreen(r.S, m.client))
	}
	return m.openDetail()
}

// openDetail pushes the appropriate detail screen for the currently
// selected row. Session rows re-fetch on Init so displayed state is
// fresh; other rows render from the cached ListXxx payload.
func (m MainScreen) openDetail() (MainScreen, tea.Cmd) {
	row := m.SelectedRow()
	if row == nil {
		return m, nil
	}
	switch r := row.(type) {
	case ProjectRow:
		return m, screen.Push(detail.NewProjectScreen(r.P))
	case AgentRow:
		return m, screen.Push(detail.NewAgentScreen(r.A))
	case ProviderRow:
		return m, screen.Push(detail.NewProviderScreen(r.P))
	case LaunchRow:
		return m, screen.Push(detail.NewLaunchScreen(r.L))
	case SessionRow:
		return m, screen.Push(detail.NewSessionScreen(r.S, m.client))
	}
	return m, nil
}

// cycleSoloChip advances the chip row through a rotating "solo"
// filter: all-on → projects-only → agents-only → providers-only →
// launches-only → sessions-only → all-on. Forward direction on Tab,
// reverse on Shift+Tab. Designed so a fresh state (all-on) cycles
// through each type and returns to all-on after len(chipOrder)+1
// presses.
func (m *MainScreen) cycleSoloChip(forward bool) {
	current := m.currentSoloChip()
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
func (m MainScreen) currentSoloChip() RowType {
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
func (m *MainScreen) setSoloChip(only RowType) {
	for _, t := range chipOrder {
		m.filters[t] = (only == "" || t == only)
	}
}

func (m MainScreen) handleChipToggle(msg tea.KeyMsg) (bool, MainScreen) {
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
func (m *MainScreen) resize() {
	base := 3 + 1 + 1 + 2
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
func (m *MainScreen) recomputeVisible() {
	pool := make([]ResultRow, 0, 64)
	for _, t := range chipOrder {
		if !m.filters[t] {
			continue
		}
		pool = append(pool, m.rowsByType[t]...)
	}
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
func (m *MainScreen) moveSelection(delta int) {
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
func (m *MainScreen) refreshBody() {
	if m.body.Width == 0 || m.body.Height == 0 {
		return
	}

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
		sb.WriteString(m.renderRowLine(r, i == m.selectedIdx))
		sb.WriteByte('\n')
	}
	m.body.SetContent(sb.String())
	m.ensureSelectedVisible()
}

// ensureSelectedVisible scrolls the viewport so that the selected row
// sits inside the visible window. One row per rendered line.
func (m *MainScreen) ensureSelectedVisible() {
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
// visible set is empty). Used by Enter-to-launch (Sprint 1) and by
// Right-arrow push-detail (Sprint 2 T-02).
func (m MainScreen) SelectedRow() ResultRow {
	if len(m.visible) == 0 {
		return nil
	}
	return m.visible[m.selectedIdx]
}

func (m MainScreen) renderRowLine(r ResultRow, selected bool) string {
	typeTag := fmt.Sprintf("[%s]", r.Type())
	body := fmt.Sprintf("%-11s %-30s  %s", typeTag, truncate(r.Title(), 30), r.Subtitle())
	if selected {
		width := m.body.Width - 4
		if width < 20 {
			width = 20
		}
		return m.theme.ResultSelected().Width(width).Render("▶ " + body)
	}
	return m.theme.ResultUnselected().Render("  " + body)
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

func (m MainScreen) renderSearch() string {
	return m.theme.Search().Width(m.width - 2).Render(m.search.View())
}

func (m MainScreen) renderChips() string {
	out := make([]string, 0, len(chipOrder))
	for i, t := range chipOrder {
		label := fmt.Sprintf("%s (⌥%d)", chipLabel(t), i+1)
		if m.filters[t] {
			out = append(out, m.theme.ChipOn().Render(label))
		} else {
			out = append(out, m.theme.ChipOff().Render(label))
		}
	}
	return m.theme.Frame().Render(strings.Join(out, ""))
}

func (m MainScreen) renderBody() string {
	return m.theme.Body().Width(m.width - 2).Render(m.body.View())
}

func (m MainScreen) renderFooter() string {
	var block strings.Builder
	for _, t := range m.toasts.items() {
		style := m.theme.ToastInfo()
		glyph := "●"
		if t.Kind == ToastError {
			style = m.theme.ToastError()
			glyph = "✗"
		}
		block.WriteString(style.Render(glyph + " " + t.Message))
		block.WriteByte('\n')
	}

	hints := []string{
		m.keyHint(m.keys.OpenDetail),
		m.keyHint(m.keys.Refresh),
		m.keyHint(m.keys.FocusSearch),
		m.keyHint(m.keys.BlurSearch),
		m.keyHint(m.keys.CycleChip),
		m.keyHint(m.keys.Quit),
		m.keyHint(m.keys.Help),
	}
	selInfo := ""
	if sel := m.SelectedRow(); sel != nil {
		enterAction := "⏎/→ detail"
		switch sel.(type) {
		case LaunchRow:
			enterAction = "⏎ launch · → detail"
		case SessionRow:
			enterAction = "⏎ attach · → detail"
		}
		selInfo = fmt.Sprintf("  ·  sel: [%s] %s — %s", sel.Type(), sel.ID(), enterAction)
	}
	status := ""
	if m.loadRemaining > 0 && m.client != nil {
		status = fmt.Sprintf("  ·  loading (%d/%d)…", len(chipOrder)-m.loadRemaining, len(chipOrder))
	} else if len(m.loadErrs) > 0 {
		status = fmt.Sprintf("  ·  %d load error(s)", len(m.loadErrs))
	} else if len(m.visible) > 0 {
		status = fmt.Sprintf("  ·  %d result(s)", len(m.visible))
	}
	block.WriteString(m.theme.Footer().Render(strings.Join(hints, "  ·  ") + status + selInfo))
	return m.theme.Frame().Render(block.String())
}

func (m MainScreen) keyHint(b key.Binding) string {
	k, help := b.Help().Key, b.Help().Desc
	return m.theme.FooterKey().Render(k) + " " + help
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
