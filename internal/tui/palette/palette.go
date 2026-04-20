// Package palette is the command palette overlay: a centered,
// keyboard-driven verb launcher triggered from any screen via `:` or
// Ctrl+K.
//
// Sprint 2 registers six verbs (view projects/agents/providers/
// launches/sessions + quit). Sprints 3/4/5 register their own via the
// same Registry without editing this package.
package palette

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"

	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// Verb is a single palette entry. Handler returns the tea.Cmd to run
// when the user picks the verb; most handlers return a batch
// containing screen.Pop() (to close the palette) plus a screen-
// specific custom msg (e.g. SetFilterMsg from the main screen).
type Verb struct {
	Name        string
	Description string
	Aliases     []string
	Handler     func() tea.Cmd
}

// searchKeys is the slice fuzzy-matched against the user's query.
// Built from Name + every alias.
func (v Verb) searchKeys() []string {
	keys := make([]string, 0, 1+len(v.Aliases))
	keys = append(keys, v.Name)
	keys = append(keys, v.Aliases...)
	return keys
}

// Registry owns the set of verbs available to the palette overlay.
// Thread-safe construction is not guaranteed; register everything
// during program wiring before the TUI starts.
type Registry struct {
	verbs []Verb
}

func NewRegistry() *Registry { return &Registry{} }

// Register appends v to the registry.
func (r *Registry) Register(v Verb) {
	r.verbs = append(r.verbs, v)
}

// Search returns verbs whose name or any alias fuzzy-matches the
// query. Empty query returns all verbs in registration order.
func (r *Registry) Search(query string) []Verb {
	q := strings.TrimSpace(query)
	if q == "" {
		out := make([]Verb, len(r.verbs))
		copy(out, r.verbs)
		return out
	}
	// Fuzzy-match against concatenated name+aliases; pick the best
	// match score per verb and use that for ordering.
	type scored struct {
		idx   int
		score int
	}
	scores := make([]scored, 0, len(r.verbs))
	for i, v := range r.verbs {
		best := -1
		for _, sk := range v.searchKeys() {
			ms := fuzzy.Find(q, []string{sk})
			if len(ms) > 0 && ms[0].Score > best {
				best = ms[0].Score
			}
		}
		if best >= 0 {
			scores = append(scores, scored{idx: i, score: best})
		}
	}
	// Sort by score descending, stable on registration order.
	for i := 1; i < len(scores); i++ {
		for j := i; j > 0 && scores[j].score > scores[j-1].score; j-- {
			scores[j], scores[j-1] = scores[j-1], scores[j]
		}
	}
	out := make([]Verb, len(scores))
	for i, s := range scores {
		out[i] = r.verbs[s.idx]
	}
	return out
}

// Overlay is the Screen implementation pushed when the user triggers
// the palette. Implements screen.Screen.
type Overlay struct {
	registry *Registry
	input    textinput.Model
	visible  []Verb
	selIdx   int
	width    int
	height   int
	theme    theme.Theme
	keys     keyBindings
}

type keyBindings struct {
	Close  key.Binding
	Select key.Binding
	Up     key.Binding
	Down   key.Binding
	Quit   key.Binding
}

func defaultKeys() keyBindings {
	return keyBindings{
		Close:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
		Select: key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "run")),
		Up:     key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "prev")),
		Down:   key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "next")),
		Quit:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

// NewOverlay constructs an Overlay wired to the given registry. The
// input field is pre-focused so the user can start typing
// immediately.
func NewOverlay(reg *Registry) Overlay {
	ti := textinput.New()
	ti.Placeholder = "Type a verb — view projects, quit, …"
	ti.Prompt = ":"
	ti.Focus()
	return Overlay{
		registry: reg,
		input:    ti,
		theme:    theme.Default(),
		keys:     defaultKeys(),
		visible:  reg.Search(""),
	}
}

func (o Overlay) Init() tea.Cmd { return textinput.Blink }

func (o Overlay) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		return o, nil

	case tea.KeyMsg:
		if key.Matches(msg, o.keys.Quit) {
			return o, tea.Quit
		}
		if key.Matches(msg, o.keys.Close) {
			return o, screen.Pop()
		}
		if key.Matches(msg, o.keys.Down) {
			if o.selIdx+1 < len(o.visible) {
				o.selIdx++
			}
			return o, nil
		}
		if key.Matches(msg, o.keys.Up) {
			if o.selIdx > 0 {
				o.selIdx--
			}
			return o, nil
		}
		if key.Matches(msg, o.keys.Select) {
			if len(o.visible) == 0 {
				return o, nil
			}
			v := o.visible[o.selIdx]
			// Pop ourselves, then run the verb's handler. Handler is
			// free to emit further cmds — including another push, a
			// custom msg, or tea.Quit.
			return o, tea.Batch(screen.Pop(), v.Handler())
		}

		prev := o.input.Value()
		var cmd tea.Cmd
		o.input, cmd = o.input.Update(msg)
		if o.input.Value() != prev {
			o.visible = o.registry.Search(o.input.Value())
			o.selIdx = 0
		}
		return o, cmd
	}
	return o, nil
}

func (o Overlay) View() string {
	if o.width == 0 || o.height == 0 {
		return ""
	}
	panelWidth := 60
	if panelWidth > o.width-4 {
		panelWidth = o.width - 4
	}
	maxResults := 8
	if len(o.visible) < maxResults {
		maxResults = len(o.visible)
	}

	// Header: the input line.
	header := o.theme.Search().Width(panelWidth - 2).Render(o.input.View())

	// Body: up to 8 verb rows.
	var body strings.Builder
	for i := 0; i < maxResults; i++ {
		v := o.visible[i]
		line := renderRow(o.theme, v, panelWidth-2, i == o.selIdx)
		body.WriteString(line)
		body.WriteByte('\n')
	}
	if maxResults == 0 {
		body.WriteString(o.theme.Footer().Render("  no matches"))
		body.WriteByte('\n')
	}

	// Footer: hints.
	hints := []string{
		o.theme.FooterKey().Render(o.keys.Select.Help().Key) + " " + o.keys.Select.Help().Desc,
		o.theme.FooterKey().Render(o.keys.Close.Help().Key) + " " + o.keys.Close.Help().Desc,
		o.theme.FooterKey().Render("↑/↓") + " nav",
	}
	footer := o.theme.Footer().Render(strings.Join(hints, "  ·  "))

	panel := lipgloss.JoinVertical(lipgloss.Left, header, body.String(), footer)
	boxed := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(o.theme.Accent()).
		Padding(0, 1).
		Render(panel)

	return lipgloss.Place(o.width, o.height, lipgloss.Center, lipgloss.Center, boxed)
}

func (o Overlay) KeyBindings() []key.Binding {
	return []key.Binding{o.keys.Select, o.keys.Close, o.keys.Up, o.keys.Down, o.keys.Quit}
}

func (o Overlay) Title() string { return "Palette" }

func renderRow(th theme.Theme, v Verb, width int, selected bool) string {
	name := v.Name
	if len(name) > 30 {
		name = name[:29] + "…"
	}
	line := "  " + padRight(name, 30) + "  " + th.Footer().Render(v.Description)
	if selected {
		return th.ResultSelected().Width(width).Render("▶ " + padRight(name, 30) + "  " + v.Description)
	}
	return line
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// TriggerKeys returns the global key bindings (`:` and Ctrl+K) that
// should open the palette. Callers (MainScreen, detail screens) embed
// this so the UX is consistent everywhere.
func TriggerKeys() (key.Binding, key.Binding) {
	colon := key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "palette"))
	ctrlK := key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "palette"))
	return colon, ctrlK
}
