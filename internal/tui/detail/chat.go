package detail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/panel"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
	"github.com/chrispian/agent-mux/pkg/claudestream"
)

// chatStreamMsg is the union sent from the attach-stream scanner
// goroutine to Update. Exactly one of ev / err / done is meaningful.
type chatStreamMsg struct {
	ev   claudestream.Event
	err  error
	done bool
}

// chatSendErrMsg surfaces a SendInput failure. Non-fatal — the input
// line is re-enabled so the user can retry the turn.
type chatSendErrMsg struct{ err error }

// chatTurn is one user→assistant exchange within the session. User is
// the prompt text (echoed locally for visibility; the daemon-side
// subprocess also sees it via stdin). Events is the ordered stream of
// parsed events observed for this turn. Done flips true on KindDone.
type chatTurn struct {
	user   string
	events []claudestream.Event
	done   bool
}

// ChatScreen renders claudestream events as a native chat surface.
// Consumes GET /sessions/{id}/attach as NDJSON and parses each line
// via pkg/claudestream.Scanner, routing typed events into Update.
//
// Key bindings:
//
//   - Esc            → detach + pop (session keeps running)
//   - Enter          → send turn when idle; toggle tool_use when focused
//   - Tab / Shift+Tab→ cycle focus between input and tool_use blocks
//   - F2             → open external terminal attached to session
//   - :              → palette
type ChatScreen struct {
	theme  theme.Theme
	client *client.Client
	s      api.SessionDTO

	width  int
	height int

	input  textinput.Model
	output viewport.Model

	history    []chatTurn
	awaiting   bool
	usageCumul claudestream.Usage
	sessionID  string // claude CLI session_id observed from system/init

	// expanded is keyed by claude tool_use block ID. Toggles via Enter
	// when focus is on that block (via Tab). Unknown IDs render collapsed.
	expanded map[string]bool

	// toolFocus indexes the list of tool_use blocks across history in
	// emission order. -1 is input-focus (the default / idle state).
	toolFocus int

	ctx    context.Context
	cancel context.CancelFunc
	ch     chan chatStreamMsg

	status   string
	finalErr error
	finished bool

	keys chatKeys
}

type chatKeys struct {
	Detach     key.Binding
	Send       key.Binding
	OpenExt    key.Binding
	Palette    key.Binding
	CycleFocus key.Binding
	CycleBack  key.Binding
}

func defaultChatKeys() chatKeys {
	return chatKeys{
		Detach:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "detach")),
		Send:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "send / toggle")),
		OpenExt:    key.NewBinding(key.WithKeys("f2"), key.WithHelp("F2", "open in shell")),
		Palette:    key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "palette")),
		CycleFocus: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus tool_use")),
		CycleBack:  key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "focus prev")),
	}
}

// NewChatScreen constructs a ChatScreen for the given session. client
// may be nil for reducer tests; when nil the stream goroutine is not
// spawned.
func NewChatScreen(s api.SessionDTO, c *client.Client) *ChatScreen {
	ti := textinput.New()
	ti.Placeholder = "type your turn, Enter to send…"
	ti.Prompt = "› "
	ti.Focus()

	vp := viewport.New(0, 0)
	vp.SetContent("Connecting…\n")

	return &ChatScreen{
		theme:     theme.Default(),
		client:    c,
		s:         s,
		input:     ti,
		output:    vp,
		expanded:  make(map[string]bool),
		toolFocus: -1,
		keys:      defaultChatKeys(),
	}
}

func (s *ChatScreen) Init() tea.Cmd {
	if s.client == nil {
		s.finished = true
		s.finalErr = fmt.Errorf("no daemon client; chat unavailable in test mode")
		return textinput.Blink
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.ch = make(chan chatStreamMsg, 64)
	go s.runStream()
	return tea.Batch(textinput.Blink, drainChatCmd(s.ch))
}

// runStream is the background goroutine that pipes daemon-attach bytes
// through the claudestream scanner and posts each parsed event onto
// s.ch. Always emits a terminal chatStreamMsg{done: true} so the
// Update loop knows to stop draining.
func (s *ChatScreen) runStream() {
	pr, pw := io.Pipe()
	// Copy attach bytes into the pipe; close the writer on return so
	// the reader-side scanner sees EOF.
	go func() {
		err := s.client.AttachStream(s.ctx, s.s.ID, pw)
		_ = pw.CloseWithError(err)
	}()

	sc := claudestream.NewScanner(pr)
	// Claude emits ~100KB assistant blocks; bump past the 64KB default
	// so oversized lines don't error the scanner mid-turn.
	sc.SetBuffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		ev, ok, err := sc.Next()
		if err != nil {
			s.emitStreamMsg(chatStreamMsg{err: err, done: true})
			return
		}
		if !ok {
			s.emitStreamMsg(chatStreamMsg{done: true})
			return
		}
		s.emitStreamMsg(chatStreamMsg{ev: ev})
	}
}

// emitStreamMsg sends m on s.ch, honoring cancellation so a detach
// doesn't block the scanner goroutine forever.
func (s *ChatScreen) emitStreamMsg(m chatStreamMsg) {
	select {
	case s.ch <- m:
	case <-s.ctx.Done():
	}
}

func (s *ChatScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.resize()
		s.refresh()
		return s, nil

	case chatStreamMsg:
		if msg.done {
			s.finished = true
			if msg.err != nil && !isBenignDetachErr(msg.err) {
				s.finalErr = msg.err
			}
			s.refresh()
			return s, nil
		}
		cmd := s.applyEvent(msg.ev)
		s.refresh()
		return s, tea.Batch(drainChatCmd(s.ch), cmd)

	case panel.PanelResponseMsg:
		// User confirmed a ui_prompt panel form. Send the response as the
		// next user turn. The agent receives it as a user message and
		// resolves its pending ui_prompt call.
		if msg.Value == "" {
			return s, nil
		}
		s.history = append(s.history, chatTurn{user: msg.Value})
		s.awaiting = true
		s.status = ""
		s.refresh()
		return s, s.sendTurnCmd(msg.Value)

	case chatSendErrMsg:
		s.status = "send failed: " + msg.err.Error()
		s.awaiting = false
		s.refresh()
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *ChatScreen) handleKey(msg tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Detach):
		if s.cancel != nil {
			s.cancel()
		}
		return s, screen.Pop()

	case key.Matches(msg, s.keys.OpenExt):
		if s.cancel != nil {
			s.cancel()
		}
		return s, tea.Batch(screen.Pop(), openExternalAttachCmd(s.s.ID))

	case key.Matches(msg, s.keys.CycleFocus):
		s.cycleFocus(true)
		s.refresh()
		return s, nil

	case key.Matches(msg, s.keys.CycleBack):
		s.cycleFocus(false)
		s.refresh()
		return s, nil

	case key.Matches(msg, s.keys.Send):
		return s.handleSendOrToggle()
	}

	// Default: forward to the text input (when it's focused).
	if s.toolFocus == -1 {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	return s, nil
}

// handleSendOrToggle routes Enter based on current focus. When focus
// is on a tool_use block, Enter toggles expand/collapse. When focus is
// on the input line, Enter sends the turn — but only if not already
// awaiting a response (see critical-state note #7).
func (s *ChatScreen) handleSendOrToggle() (screen.Screen, tea.Cmd) {
	if s.toolFocus >= 0 {
		if id := s.focusedToolID(); id != "" {
			s.expanded[id] = !s.expanded[id]
			s.refresh()
		}
		return s, nil
	}

	if s.awaiting {
		s.status = "waiting for response…"
		s.refresh()
		return s, nil
	}
	line := strings.TrimSpace(s.input.Value())
	if line == "" {
		return s, nil
	}
	s.input.SetValue("")
	s.history = append(s.history, chatTurn{user: line})
	s.awaiting = true
	s.status = ""
	s.refresh()
	return s, s.sendTurnCmd(line)
}

// applyEvent updates chat state from one parsed claudestream event.
// Events land on the *current* (last) turn for Delta/ToolUse/Usage/Done;
// SessionID and Error are session-scoped rather than turn-scoped but
// are still appended to the active turn so they render in the
// conversation flow.
func (s *ChatScreen) applyEvent(ev claudestream.Event) tea.Cmd {
	// Session-scoped side-effects first. The non-listed kinds (Delta,
	// ToolUse, Error) have no session-level effect — they're rendered
	// inline via renderHistory.
	switch ev.Kind { //nolint:exhaustive // intentional subset: session-scoped kinds only
	case claudestream.KindSessionID:
		if s.sessionID == "" {
			s.sessionID = ev.SessionID
		}
	case claudestream.KindUsage:
		if ev.Usage != nil {
			s.usageCumul.InputTokens += ev.Usage.InputTokens
			s.usageCumul.OutputTokens += ev.Usage.OutputTokens
			s.usageCumul.CacheCreationTokens += ev.Usage.CacheCreationTokens
			s.usageCumul.CacheReadTokens += ev.Usage.CacheReadTokens
			s.usageCumul.StopReason = ev.Usage.StopReason
		}
	case claudestream.KindDone:
		s.awaiting = false
		if i := len(s.history) - 1; i >= 0 {
			s.history[i].done = true
		}
		// Clear any stale status (e.g., "waiting for response…") now
		// that the turn has closed cleanly.
		if strings.HasPrefix(s.status, "waiting") {
			s.status = ""
		}
	case claudestream.KindUIPrompt:
		if ev.UIPrompt == nil {
			return nil
		}
		content := buildPanelContent(ev)
		if content == nil {
			return nil
		}
		return func() tea.Msg {
			return panel.PanelPushMsg{Content: content}
		}
	}

	// If we've received an event before the first user turn was
	// synthesized locally (e.g., the server-side session was
	// pre-existing or the user attached mid-turn), synthesize a
	// placeholder turn so the event has a container.
	if len(s.history) == 0 {
		s.history = append(s.history, chatTurn{})
	}
	idx := len(s.history) - 1
	s.history[idx].events = append(s.history[idx].events, ev)
	return nil
}

// sendTurnCmd wraps SendInput in a tea.Cmd. The daemon-side adapter
// treats the payload as the user prompt for the next claude -p run.
func (s *ChatScreen) sendTurnCmd(line string) tea.Cmd {
	if s.client == nil {
		return nil
	}
	id := s.s.ID
	c := s.client
	return func() tea.Msg {
		if err := c.SendInput(context.Background(), id, []byte(line)); err != nil {
			return chatSendErrMsg{err: err}
		}
		return nil
	}
}

// cycleFocus advances toolFocus through [-1, 0, 1, … nBlocks-1] and
// wraps. -1 is input-focus. forward=true increments; forward=false
// decrements.
func (s *ChatScreen) cycleFocus(forward bool) {
	n := s.toolBlockCount()
	if n == 0 {
		s.toolFocus = -1
		return
	}
	// States: -1, 0, 1, …, n-1. Length n+1.
	states := n + 1
	cur := s.toolFocus + 1 // shift so -1→0, 0→1, …
	if forward {
		cur = (cur + 1) % states
	} else {
		cur = (cur - 1 + states) % states
	}
	s.toolFocus = cur - 1
	// Un-focus the input when a tool_use gains focus; re-focus on return.
	if s.toolFocus == -1 {
		s.input.Focus()
	} else {
		s.input.Blur()
	}
}

// toolBlockCount returns the total number of tool_use blocks across
// history in emission order.
func (s *ChatScreen) toolBlockCount() int {
	n := 0
	for _, t := range s.history {
		for _, ev := range t.events {
			if ev.Kind == claudestream.KindToolUse {
				n++
			}
		}
	}
	return n
}

// focusedToolID returns the claude tool_use block ID at toolFocus, or
// "" if the focus points nowhere (input-focus or out-of-range).
func (s *ChatScreen) focusedToolID() string {
	if s.toolFocus < 0 {
		return ""
	}
	seen := 0
	for _, t := range s.history {
		for _, ev := range t.events {
			if ev.Kind != claudestream.KindToolUse {
				continue
			}
			if seen == s.toolFocus {
				if ev.ToolUse != nil {
					return ev.ToolUse.ID
				}
				return ""
			}
			seen++
		}
	}
	return ""
}

// resize recomputes viewport dimensions. Overhead budget per critical-
// state note #10: header 1 + bordered input 3 + footer 1 + body border 2 = 7.
func (s *ChatScreen) resize() {
	const verticalOverhead = 1 + 3 + 1 + 2
	bodyHeight := s.height - verticalOverhead
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	bodyWidth := s.width - 2
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	s.output.Width = bodyWidth
	s.output.Height = bodyHeight
	s.input.Width = bodyWidth - 4
}

// refresh rebuilds the viewport content from history + state. Pinned
// to bottom so new events stay visible.
func (s *ChatScreen) refresh() {
	s.output.SetContent(s.renderHistory())
	s.output.GotoBottom()
}

// renderHistory produces the viewport body. Each turn renders the
// user prompt then events in order; tool_use blocks render as
// one-liners or expanded JSON based on s.expanded.
func (s *ChatScreen) renderHistory() string {
	if len(s.history) == 0 {
		return "Connecting…\n"
	}
	var sb strings.Builder
	toolSeen := 0
	for ti, t := range s.history {
		if ti > 0 {
			sb.WriteString("\n")
		}
		if t.user != "" {
			sb.WriteString(s.theme.Footer().Render("› " + t.user))
			sb.WriteString("\n")
		}
		// Deltas concatenate into a single assistant block for
		// readability; non-delta events render inline.
		var delta strings.Builder
		for _, ev := range t.events {
			switch ev.Kind {
			case claudestream.KindDelta:
				delta.WriteString(ev.Text)
			case claudestream.KindToolUse:
				if delta.Len() > 0 {
					sb.WriteString(delta.String())
					sb.WriteString("\n")
					delta.Reset()
				}
				sb.WriteString(s.renderToolUse(ev, toolSeen))
				sb.WriteString("\n")
				toolSeen++
			case claudestream.KindSessionID:
				sb.WriteString(s.theme.Footer().Render(
					fmt.Sprintf("— session %s —", shortID(ev.SessionID))))
				sb.WriteString("\n")
			case claudestream.KindError:
				sb.WriteString(errorStyle().Render("error: " + ev.ErrorMsg))
				sb.WriteString("\n")
			case claudestream.KindUIPrompt:
				// Rendered in the side panel; suppress inline.
			case claudestream.KindUsage, claudestream.KindDone:
				// Rendered in the header / footer; suppress inline.
			}
		}
		if delta.Len() > 0 {
			sb.WriteString(delta.String())
			sb.WriteString("\n")
		}
	}
	if s.finished {
		sb.WriteString("\n")
		if s.finalErr != nil {
			sb.WriteString(errorStyle().Render("[stream closed: " + s.finalErr.Error() + "]"))
		} else {
			sb.WriteString(s.theme.Footer().Render("[detached]"))
		}
	}
	return sb.String()
}

func (s *ChatScreen) renderToolUse(ev claudestream.Event, idx int) string {
	name := "unknown"
	id := ""
	if ev.ToolUse != nil {
		name = ev.ToolUse.Name
		id = ev.ToolUse.ID
	}
	marker := "▸"
	if id != "" && s.expanded[id] {
		marker = "▾"
	}
	prefix := fmt.Sprintf("%s [tool: %s]", marker, name)
	if s.toolFocus == idx {
		prefix = s.theme.ResultSelected().Render(prefix)
	}
	if id == "" || !s.expanded[id] {
		return prefix
	}
	// Expanded: pretty-print the input JSON.
	body := "(no input)"
	if ev.ToolUse != nil && len(ev.ToolUse.Input) > 0 {
		if pretty, err := json.MarshalIndent(ev.ToolUse.Input, "    ", "  "); err == nil {
			body = "    " + string(pretty)
		}
	}
	return prefix + "\n" + body
}

func (s *ChatScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	header := s.theme.Header().Width(s.width - 2).Render(s.headerLine())
	bodyFrame := s.theme.Body().Width(s.width - 2).Render(s.output.View())
	inputFrame := s.theme.Search().Width(s.width - 2).Render(s.inputLine())
	mid := lipgloss.JoinVertical(lipgloss.Left, bodyFrame, inputFrame)

	hints := []string{
		s.keyHint(s.keys.Send),
		s.keyHint(s.keys.CycleFocus),
		s.keyHint(s.keys.Detach),
		s.keyHint(s.keys.OpenExt),
		s.keyHint(s.keys.Palette),
	}
	statusSuffix := ""
	if s.status != "" {
		statusSuffix = "  ·  " + s.status
	}
	footer := s.theme.Frame().Render(
		s.theme.Footer().Render(strings.Join(hints, "  ·  ") + statusSuffix),
	)
	return layout.RenderDetail(header, mid, footer, s.width, s.height)
}

func (s *ChatScreen) headerLine() string {
	sid := shortID(s.s.ID)
	tokens := fmt.Sprintf("in:%d out:%d cache:%d",
		s.usageCumul.InputTokens, s.usageCumul.OutputTokens, s.usageCumul.CacheReadTokens)
	state := "idle"
	switch {
	case s.finished:
		state = "detached"
	case s.awaiting:
		state = "⋯ awaiting"
	}
	return fmt.Sprintf("Chat %s · %s · %s · %s", sid, s.s.Workspace, tokens, state)
}

func (s *ChatScreen) inputLine() string {
	if s.awaiting {
		return s.theme.Footer().Render("(waiting for response — Enter disabled)")
	}
	if s.toolFocus >= 0 {
		return s.theme.Footer().Render("(focused on tool_use block — ⏎ toggle, Tab to return)")
	}
	return s.input.View()
}

func (s *ChatScreen) keyHint(k key.Binding) string {
	hk, desc := k.Help().Key, k.Help().Desc
	return s.theme.FooterKey().Render(hk) + " " + desc
}

func (s *ChatScreen) KeyBindings() []key.Binding {
	return []key.Binding{s.keys.Send, s.keys.CycleFocus, s.keys.Detach, s.keys.OpenExt, s.keys.Palette}
}

func (s *ChatScreen) Title() string {
	return "Chat " + shortID(s.s.ID)
}

// drainChatCmd returns a tea.Cmd that blocks on ch for one message.
// On channel close returns a terminal done msg.
func drainChatCmd(ch <-chan chatStreamMsg) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-ch
		if !ok {
			return chatStreamMsg{done: true}
		}
		return m
	}
}

// buildPanelContent converts a KindUIPrompt event into the appropriate
// panel.Content type. Returns nil for unknown kinds (panel stays closed).
func buildPanelContent(ev claudestream.Event) panel.Content {
	if ev.UIPrompt == nil {
		return nil
	}
	d := ev.UIPrompt
	switch d.Kind {
	case "yes_no":
		def := "yes"
		if s, ok := d.Default.(string); ok {
			def = s
		}
		return panel.NewYesNoContent(d.Title, def, d.ToolUseID)
	case "multi_choice":
		idx := 0
		if f, ok := d.Default.(float64); ok { // JSON numbers unmarshal as float64
			idx = int(f)
		}
		return panel.NewMultiChoiceContent(d.Title, d.Options, idx, d.ToolUseID)
	case "text_input":
		def := ""
		if s, ok := d.Default.(string); ok {
			def = s
		}
		return panel.NewTextInputContent(d.Title, def, d.ToolUseID)
	case "review_card", "diff", "document":
		body := d.Body
		if body == "" {
			body = d.Title
		}
		return panel.NewViewportContentWithToolUse(body, d.ToolUseID)
	default:
		return nil
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func errorStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#ff6b6b"))
}
