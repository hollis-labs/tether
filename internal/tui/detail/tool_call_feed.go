package detail

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ToolCallFeedScreen is a live scrolling panel showing all proxied tool call
// events. Events are pushed to it by the root Model via ReceiveProxyEvents —
// the screen does NO polling of its own. This keeps the event stream alive
// regardless of whether the feed is currently the top screen.
//
// Scrolling up pauses auto-scroll; pressing 'f' or reaching the bottom
// resumes following.
type ToolCallFeedScreen struct {
	theme  theme.Theme
	body   viewport.Model
	width  int
	height int

	// evts is the current snapshot rendered in View.
	evts []api.ProxyEventDTO

	// following controls auto-scroll: true = pin to bottom on new events.
	following bool

	keys feedKeys
}

type feedKeys struct {
	Back     key.Binding
	Follow   key.Binding
	Quit     key.Binding
	ScrollUp key.Binding
	ScrollDn key.Binding
}

func defaultFeedKeys() feedKeys {
	return feedKeys{
		Back:     key.NewBinding(key.WithKeys("esc", "left"), key.WithHelp("esc", "back")),
		Follow:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		ScrollUp: key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
		ScrollDn: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
	}
}

// NewToolCallFeedScreen creates a feed screen. Events are delivered via
// ReceiveProxyEvents from the root Model rather than polled here.
func NewToolCallFeedScreen() *ToolCallFeedScreen {
	return &ToolCallFeedScreen{
		theme:     theme.Default(),
		body:      viewport.New(0, 0),
		following: true,
		keys:      defaultFeedKeys(),
	}
}

// Title implements screen.Screen.
func (s *ToolCallFeedScreen) Title() string { return "Tool Call Feed" }

// KeyBindings implements screen.Screen.
func (s *ToolCallFeedScreen) KeyBindings() []key.Binding {
	return []key.Binding{
		s.keys.Back,
		s.keys.Follow,
		s.keys.ScrollUp,
		s.keys.ScrollDn,
		s.keys.Quit,
	}
}

// Init implements screen.Screen. No polling is started here; the root
// model drives updates via ReceiveProxyEvents.
func (s *ToolCallFeedScreen) Init() tea.Cmd { return nil }

// ReceiveProxyEvents is called by the root Model when a fresh event
// snapshot arrives. The screen updates its local slice and redraws.
func (s *ToolCallFeedScreen) ReceiveProxyEvents(evts []api.ProxyEventDTO) (screen.Screen, tea.Cmd) {
	s.evts = evts
	s.refreshBody()
	return s, nil
}

// Update implements screen.Screen.
func (s *ToolCallFeedScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		s.resizeViewport()
		s.refreshBody()
		return s, nil

	case tea.KeyMsg:
		switch {
		case key.Matches(m, s.keys.Quit):
			return s, tea.Quit
		case key.Matches(m, s.keys.Back):
			return s, screen.Pop()
		case key.Matches(m, s.keys.Follow):
			s.following = true
			s.scrollToBottom()
			return s, nil
		case key.Matches(m, s.keys.ScrollUp):
			s.following = false
			s.body.ScrollUp(3)
			return s, nil
		case key.Matches(m, s.keys.ScrollDn):
			s.body.ScrollDown(3)
			if s.body.AtBottom() {
				s.following = true
			}
			return s, nil
		case m.Type == tea.KeyPgUp:
			s.following = false
			s.body.HalfPageUp()
			return s, nil
		case m.Type == tea.KeyPgDown:
			s.body.HalfPageDown()
			if s.body.AtBottom() {
				s.following = true
			}
			return s, nil
		}
	}
	return s, nil
}

// View implements screen.Screen.
func (s *ToolCallFeedScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	header := s.theme.Header().Width(s.width - 2).Render(s.headerText())
	body := s.theme.Body().Width(s.width - 2).Render(s.body.View())
	footer := s.renderFooter()
	return layout.RenderDetail(header, body, footer, s.width, s.height)
}

// ─── internal helpers ─────────────────────────────────────────────────────────

func (s *ToolCallFeedScreen) refreshBody() {
	if s.body.Width == 0 || s.body.Height == 0 {
		return
	}
	var sb strings.Builder
	if len(s.evts) == 0 {
		sb.WriteString("\n  No tool call events yet.\n\n")
		sb.WriteString("  Make a tool call through the MCP relay (mux mcp --proxy --broker)\n")
		sb.WriteString("  to see events appear here in real time.\n")
	} else {
		for _, ev := range s.evts {
			sb.WriteString(s.renderEventRow(ev))
			sb.WriteByte('\n')
		}
	}
	s.body.SetContent(sb.String())
	if s.following {
		s.scrollToBottom()
	}
}

func (s *ToolCallFeedScreen) renderEventRow(ev api.ProxyEventDTO) string {
	ts := "(unknown)"
	if t, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err == nil {
		ts = t.Local().Format("15:04:05")
	} else if t, err := time.Parse(time.RFC3339, ev.Timestamp); err == nil {
		ts = t.Local().Format("15:04:05")
	}

	var okGlyph string
	var okStyle lipgloss.Style
	if ev.OK {
		okGlyph = "+"
		okStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#007700", Dark: "#9ECE6A"})
	} else {
		okGlyph = "!"
		okStyle = lipgloss.NewStyle().Foreground(s.theme.Danger())
	}

	server := truncateFeed(ev.Server, 10)
	tool := truncateFeed(ev.ToolName, 30)
	dur := fmt.Sprintf("%dms", ev.DurationMs)

	row := fmt.Sprintf("[%s] %s  %-10s  %-30s  %6s",
		ts,
		okStyle.Render(okGlyph),
		server,
		tool,
		dur,
	)

	if !ev.OK && ev.Error != "" {
		errTxt := ev.Error
		if len(errTxt) > 60 {
			errTxt = errTxt[:57] + "..."
		}
		errStyle := lipgloss.NewStyle().Foreground(s.theme.Danger())
		row += "  " + errStyle.Render(errTxt)
	}

	return row
}

func (s *ToolCallFeedScreen) scrollToBottom() { s.body.GotoBottom() }

func (s *ToolCallFeedScreen) resizeViewport() {
	const overhead = 1 + 1 + 2
	h := s.height - overhead
	if h < 3 {
		h = 3
	}
	w := s.width - 2
	if w < 20 {
		w = 20
	}
	s.body.Width = w
	s.body.Height = h
}

func (s *ToolCallFeedScreen) headerText() string {
	following := "  [auto-scroll ON]"
	if !s.following {
		following = "  [auto-scroll paused — press f to resume]"
	}
	return fmt.Sprintf("Tool Call Feed — %d events%s", len(s.evts), following)
}

func (s *ToolCallFeedScreen) renderFooter() string {
	hints := []string{
		keyHintFeed(s.keys.Back),
		keyHintFeed(s.keys.Follow),
		keyHintFeed(s.keys.ScrollUp),
		keyHintFeed(s.keys.ScrollDn),
		keyHintFeed(s.keys.Quit),
	}
	th := s.theme
	return th.Frame().Render(
		th.Footer().Render(strings.Join(hints, "  ·  ")),
	)
}

func keyHintFeed(b key.Binding) string {
	th := theme.Default()
	k, desc := b.Help().Key, b.Help().Desc
	return th.FooterKey().Render(k) + " " + desc
}

func truncateFeed(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
