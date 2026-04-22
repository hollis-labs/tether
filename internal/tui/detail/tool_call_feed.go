package detail

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/mcpadapter"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

const (
	// feedMaxEvents is the maximum number of events kept in the feed buffer.
	feedMaxEvents = 200

	// feedPollInterval is how often the feed polls the event store.
	feedPollInterval = 100 * time.Millisecond
)

// toolCallFeedTickMsg is emitted by the 100ms ticker to trigger a store poll.
type toolCallFeedTickMsg struct{}

// ToolCallFeedScreen is a live scrolling panel showing all proxied tool call
// events. It polls a ToolCallEventStore every 100ms and auto-scrolls to the
// latest event. Scrolling up pauses auto-scroll; pressing 'f' or scrolling
// to the bottom resumes following.
//
// This screen is added as a navigable screen in the existing detail stack —
// accessible via the main screen or a future keyboard shortcut.
//
// Phase 3 will add per-session filtering; Phase 2 shows all sessions.
type ToolCallFeedScreen struct {
	theme  theme.Theme
	store  *mcpadapter.ToolCallEventStore
	body   viewport.Model
	width  int
	height int

	// events is the local snapshot rendered in View.
	evts []events.ToolCallEvent

	// following controls auto-scroll: true = pin to bottom on every tick.
	following bool

	keys feedKeys
}

type feedKeys struct {
	Back      key.Binding
	Follow    key.Binding
	Quit      key.Binding
	ScrollUp  key.Binding
	ScrollDn  key.Binding
}

func defaultFeedKeys() feedKeys {
	return feedKeys{
		Back:   key.NewBinding(key.WithKeys("esc", "left"), key.WithHelp("esc", "back")),
		Follow: key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Quit:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		ScrollUp: key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
		ScrollDn: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
	}
}

// NewToolCallFeedScreen creates a feed screen backed by store. store may be
// nil (read-only empty view); useful for tests and offline TUI sessions.
func NewToolCallFeedScreen(store *mcpadapter.ToolCallEventStore) *ToolCallFeedScreen {
	return &ToolCallFeedScreen{
		theme:     theme.Default(),
		store:     store,
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

// Init implements screen.Screen — start the polling ticker.
func (s *ToolCallFeedScreen) Init() tea.Cmd {
	return s.tickCmd()
}

func (s *ToolCallFeedScreen) tickCmd() tea.Cmd {
	return tea.Tick(feedPollInterval, func(time.Time) tea.Msg {
		return toolCallFeedTickMsg{}
	})
}

// Update implements screen.Screen.
func (s *ToolCallFeedScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		s.resizeViewport()
		s.refreshBody()
		return s, nil

	case toolCallFeedTickMsg:
		s.poll()
		return s, s.tickCmd()

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
			s.body.LineUp(3)
			return s, nil
		case key.Matches(m, s.keys.ScrollDn):
			s.body.LineDown(3)
			// Resume following when the user scrolls all the way to the bottom.
			if s.body.AtBottom() {
				s.following = true
			}
			return s, nil
		case m.Type == tea.KeyPgUp:
			s.following = false
			s.body.HalfViewUp()
			return s, nil
		case m.Type == tea.KeyPgDown:
			s.body.HalfViewDown()
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

func (s *ToolCallFeedScreen) poll() {
	if s.store == nil {
		return
	}
	evts := s.store.Query(mcpadapter.ToolCallEventFilter{Limit: feedMaxEvents})
	s.evts = evts
	s.refreshBody()
}

func (s *ToolCallFeedScreen) refreshBody() {
	if s.body.Width == 0 || s.body.Height == 0 {
		return
	}
	var sb strings.Builder
	if len(s.evts) == 0 {
		sb.WriteString("\n  No tool call events yet.\n\n")
		sb.WriteString("  Start mux mcp --proxy and make a proxied tool call\n")
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

func (s *ToolCallFeedScreen) renderEventRow(ev events.ToolCallEvent) string {
	ts := ev.Timestamp.Local().Format("15:04:05")

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

func (s *ToolCallFeedScreen) scrollToBottom() {
	s.body.GotoBottom()
}

func (s *ToolCallFeedScreen) resizeViewport() {
	// overhead: 1 header row + 1 footer row + 2 border rows (top+bottom of body box)
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
