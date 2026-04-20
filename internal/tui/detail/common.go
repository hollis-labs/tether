package detail

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// keyBindings is the common navigation set for every detail screen
// in Sprint 2. Sprint 3 adds edit/delete; T-04 / T-05 add session-
// specific actions on top of this base.
type keyBindings struct {
	Back    key.Binding
	Palette key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func defaultKeyBindings() keyBindings {
	return keyBindings{
		Back: key.NewBinding(
			key.WithKeys("esc", "left"),
			key.WithHelp("esc", "back"),
		),
		Palette: key.NewBinding(
			key.WithKeys(":"),
			key.WithHelp(":", "palette"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
	}
}

// base is embedded in every concrete detail screen. It owns the
// rendering frame (header + viewport + footer) and the common
// navigation keys. Concrete screens populate title + fields and
// implement any extra keys on top.
type base struct {
	title  string
	theme  theme.Theme
	keys   keyBindings
	width  int
	height int
	body   viewport.Model
	extra  []key.Binding // screen-specific key hints for the footer
}

func newBase(title string) base {
	return base{
		title: title,
		theme: theme.Default(),
		keys:  defaultKeyBindings(),
		body:  viewport.New(0, 0),
	}
}

// setContent resizes the viewport for the current width/height and
// populates it with rendered content.
func (b *base) setContent(content string) {
	const verticalOverhead = 1 + 1 + 2 // header + footer + body border
	bodyHeight := b.height - verticalOverhead
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	bodyWidth := b.width - 2
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	b.body.Width = bodyWidth
	b.body.Height = bodyHeight
	b.body.SetContent(content)
}

// updateCommon returns (cmd, true) when msg was handled by the
// shared reducer (window resize, back/Ctrl-C, viewport scroll).
// Concrete screens check this first; on handled=true they return
// (screen, cmd) directly.
func (b *base) updateCommon(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		b.width, b.height = msg.Width, msg.Height
		return nil, true

	case tea.KeyMsg:
		if key.Matches(msg, b.keys.Back) {
			return screen.Pop(), true
		}
		if msg.Type == tea.KeyCtrlC {
			return tea.Quit, true
		}
		switch msg.Type { //nolint:exhaustive // intentional subset: scroll keys only
		case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
			var cmd tea.Cmd
			b.body, cmd = b.body.Update(msg)
			return cmd, true
		}
	}
	return nil, false
}

// render builds the header/body/footer frame.
func (b base) render() string {
	if b.width == 0 || b.height == 0 {
		return ""
	}
	header := b.theme.Header().Width(b.width - 2).Render(b.title)
	body := b.theme.Body().Width(b.width - 2).Render(b.body.View())
	footer := b.renderFooter()
	return layout.RenderDetail(header, body, footer, b.width, b.height)
}

func (b base) renderFooter() string {
	hints := []string{
		b.keyHint(b.keys.Back),
	}
	for _, e := range b.extra {
		hints = append(hints, b.keyHint(e))
	}
	hints = append(hints,
		b.keyHint(b.keys.Palette),
		b.keyHint(b.keys.Help),
		b.keyHint(b.keys.Quit),
	)
	return b.theme.Frame().Render(
		b.theme.Footer().Render(strings.Join(hints, "  ·  ")),
	)
}

func (b base) keyHint(k key.Binding) string {
	hk, desc := k.Help().Key, k.Help().Desc
	return b.theme.FooterKey().Render(hk) + " " + desc
}

// bindings returns the screen's full key list in footer order; used
// by each concrete screen's KeyBindings() implementation.
func (b base) bindings() []key.Binding {
	out := make([]key.Binding, 0, 4+len(b.extra))
	out = append(out, b.keys.Back)
	out = append(out, b.extra...)
	out = append(out, b.keys.Palette, b.keys.Help, b.keys.Quit)
	return out
}
