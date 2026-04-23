package detail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

type messageMode int

const (
	messageModeView messageMode = iota
	messageModeRead
	messageModeAddress
	messageModeReply
)

type messageAction int

const (
	messageActionRead messageAction = iota
	messageActionReply
	messageActionRefresh
	messageActionAddress
	messageActionConsume
)

// MessagesScreen is a native Mux inbox surface. It talks directly to muxd's
// /messages endpoints and keeps fetched envelopes in memory because the daemon
// currently marks inbox reads delivered.
type MessagesScreen struct {
	theme theme.Theme
	body  viewport.Model
	keys  messageKeys

	client *client.Client

	width  int
	height int
	mode   messageMode

	inboxAddress string
	addressInput textinput.Model
	replyInput   textinput.Model

	messages []client.MessageEnvelope
	selected int
	action   messageAction
	loading  bool
	err      string
	status   string
}

type messageKeys struct {
	Back       key.Binding
	Primary    key.Binding
	NextAction key.Binding
	PrevAction key.Binding
	ScrollUp   key.Binding
	ScrollDown key.Binding
	Quit       key.Binding
}

func defaultMessageKeys() messageKeys {
	return messageKeys{
		Back:       key.NewBinding(key.WithKeys("esc", "left"), key.WithHelp("esc", "back")),
		Primary:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "run")),
		NextAction: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "action")),
		PrevAction: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧tab", "action")),
		ScrollUp:   key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
		ScrollDown: key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
		Quit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

// NewMessagesScreen creates the inbox view. The default address can be set with
// AGENT_MUX_TUI_INBOX or MUX_TUI_INBOX; otherwise msg://user/local/me is used.
func NewMessagesScreen(c *client.Client) *MessagesScreen {
	addr := os.Getenv("AGENT_MUX_TUI_INBOX")
	if strings.TrimSpace(addr) == "" {
		addr = os.Getenv("MUX_TUI_INBOX")
	}
	if strings.TrimSpace(addr) == "" {
		addr = "msg://user/local/me"
	}

	addressInput := textinput.New()
	addressInput.Prompt = "  "
	addressInput.Placeholder = "msg://user/local/me"
	addressInput.SetValue(addr)

	replyInput := textinput.New()
	replyInput.Prompt = "  "
	replyInput.Placeholder = "Type reply, then Enter"

	return &MessagesScreen{
		theme:        theme.Default(),
		body:         viewport.New(0, 0),
		keys:         defaultMessageKeys(),
		client:       c,
		inboxAddress: addr,
		addressInput: addressInput,
		replyInput:   replyInput,
	}
}

func (s *MessagesScreen) Title() string { return "Messages" }

func (s *MessagesScreen) KeyBindings() []key.Binding {
	return []key.Binding{
		s.keys.Back,
		s.keys.Primary,
		s.keys.NextAction,
		s.keys.PrevAction,
		s.keys.ScrollUp,
		s.keys.ScrollDown,
		s.keys.Quit,
	}
}

func (s *MessagesScreen) Init() tea.Cmd {
	if s.client == nil {
		s.status = "No daemon client configured."
		return nil
	}
	s.loading = true
	return tea.Batch(textinput.Blink, loadMuxMessagesCmd(s.client, s.inboxAddress))
}

func (s *MessagesScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		s.resize()
		s.refreshBody()
		return s, nil

	case muxMessagesLoadedMsg:
		s.loading = false
		if m.err != nil {
			s.err = m.err.Error()
			s.status = ""
		} else {
			s.err = ""
			added := s.mergeMessages(m.messages)
			s.status = fmt.Sprintf("Loaded %d new message%s.", added, plural(added))
		}
		s.refreshBody()
		return s, nil

	case muxMessageActionMsg:
		if m.err != nil {
			s.err = m.err.Error()
			s.status = ""
		} else {
			s.err = ""
			s.status = m.status
			if m.consumedID != "" {
				s.markConsumed(m.consumedID)
			}
		}
		s.mode = messageModeView
		s.replyInput.SetValue("")
		s.refreshBody()
		return s, nil

	case tea.KeyMsg:
		switch s.mode {
		case messageModeRead:
			return s.updateReadMode(m)
		case messageModeAddress:
			return s.updateAddressMode(m)
		case messageModeReply:
			return s.updateReplyMode(m)
		default:
			return s.updateViewMode(m)
		}
	}
	return s, nil
}

func (s *MessagesScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	header := s.theme.Header().Width(s.width - 2).Render("Messages · " + s.inboxAddress)
	body := s.theme.Body().Width(s.width - 2).Render(s.body.View())
	footer := s.renderFooter()
	return layout.RenderDetail(header, body, footer, s.width, s.height)
}

func (s *MessagesScreen) updateViewMode(m tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(m, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(m, s.keys.Back):
		return s, screen.Pop()
	case key.Matches(m, s.keys.NextAction):
		s.cycleAction(1)
		s.refreshBody()
		return s, nil
	case key.Matches(m, s.keys.PrevAction):
		s.cycleAction(-1)
		s.refreshBody()
		return s, nil
	case key.Matches(m, s.keys.Primary):
		return s.runAction()
	case key.Matches(m, s.keys.ScrollUp):
		s.moveSelection(-1)
		s.refreshBody()
		return s, nil
	case key.Matches(m, s.keys.ScrollDown):
		s.moveSelection(1)
		s.refreshBody()
		return s, nil
	case m.Type == tea.KeyPgUp:
		s.body.HalfPageUp()
		return s, nil
	case m.Type == tea.KeyPgDown:
		s.body.HalfPageDown()
		return s, nil
	}
	return s, nil
}

func (s *MessagesScreen) updateReadMode(m tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(m, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(m, s.keys.Back):
		s.mode = messageModeView
		s.refreshBody()
		return s, nil
	case key.Matches(m, s.keys.ScrollUp):
		s.body.ScrollUp(3)
		return s, nil
	case key.Matches(m, s.keys.ScrollDown):
		s.body.ScrollDown(3)
		return s, nil
	case m.Type == tea.KeyPgUp:
		s.body.HalfPageUp()
		return s, nil
	case m.Type == tea.KeyPgDown:
		s.body.HalfPageDown()
		return s, nil
	}
	return s, nil
}

func (s *MessagesScreen) updateAddressMode(m tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(m, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(m, s.keys.Back):
		s.mode = messageModeView
		s.addressInput.Blur()
		s.refreshBody()
		return s, nil
	case m.Type == tea.KeyEnter:
		next := strings.TrimSpace(s.addressInput.Value())
		if next == "" {
			return s, nil
		}
		s.inboxAddress = next
		s.mode = messageModeView
		s.addressInput.Blur()
		s.loading = true
		s.status = "Loading inbox..."
		s.refreshBody()
		if s.client == nil {
			s.loading = false
			s.status = "No daemon client configured."
			return s, nil
		}
		return s, loadMuxMessagesCmd(s.client, s.inboxAddress)
	}
	var cmd tea.Cmd
	s.addressInput, cmd = s.addressInput.Update(m)
	s.refreshBody()
	return s, cmd
}

func (s *MessagesScreen) updateReplyMode(m tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(m, s.keys.Quit):
		return s, tea.Quit
	case key.Matches(m, s.keys.Back):
		s.mode = messageModeView
		s.replyInput.Blur()
		s.refreshBody()
		return s, nil
	case m.Type == tea.KeyEnter:
		msg := s.selectedMessage()
		text := strings.TrimSpace(s.replyInput.Value())
		if msg == nil || text == "" || s.client == nil {
			return s, nil
		}
		s.status = "Sending reply..."
		s.refreshBody()
		return s, replyToMuxMessageCmd(s.client, s.inboxAddress, *msg, text)
	}
	var cmd tea.Cmd
	s.replyInput, cmd = s.replyInput.Update(m)
	s.refreshBody()
	return s, cmd
}

func (s *MessagesScreen) resize() {
	bodyHeight := s.height - 4
	if bodyHeight < 3 {
		bodyHeight = 3
	}
	bodyWidth := s.width - 2
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	s.body.Width = bodyWidth
	s.body.Height = bodyHeight
	s.addressInput.Width = bodyWidth - 6
	s.replyInput.Width = bodyWidth - 6
}

func (s *MessagesScreen) refreshBody() {
	if s.body.Width == 0 || s.body.Height == 0 {
		return
	}
	var b strings.Builder
	switch s.mode {
	case messageModeAddress:
		b.WriteString("\n  Inbox address\n\n")
		b.WriteString(s.addressInput.View())
		b.WriteString("\n\n  Enter saves and reloads. Esc cancels.\n")
	case messageModeRead:
		if msg := s.selectedMessage(); msg != nil {
			b.WriteString(s.renderSelected(*msg))
			b.WriteString("\n  Esc returns to inbox.\n")
		} else {
			b.WriteString("\n  No message selected.\n")
		}
	case messageModeReply:
		msg := s.selectedMessage()
		if msg != nil {
			b.WriteString("\n  Reply to ")
			b.WriteString(msg.From)
			b.WriteString("\n\n")
		}
		b.WriteString(s.replyInput.View())
		b.WriteString("\n\n  Enter sends and consumes the selected message. Esc cancels.\n")
	default:
		if s.loading {
			b.WriteString("  Loading inbox...\n\n")
		}
		if s.err != "" {
			b.WriteString(lipgloss.NewStyle().Foreground(s.theme.Danger()).Render("  "+s.err) + "\n\n")
		} else if s.status != "" {
			b.WriteString("  " + s.status + "\n\n")
		}
		if len(s.messages) == 0 {
			b.WriteString("  No messages for this address.\n\n")
			b.WriteString("  Tab chooses an action. Enter runs it.\n")
		} else {
			for i, msg := range s.messages {
				b.WriteString(s.renderMessageRow(msg, i == s.selected))
				b.WriteByte('\n')
			}
			if msg := s.selectedMessage(); msg != nil {
				b.WriteString("\n")
				b.WriteString(s.renderSelected(*msg))
			}
		}
	}
	s.body.SetContent(b.String())
	s.ensureSelectedVisible()
}

func (s *MessagesScreen) renderMessageRow(msg client.MessageEnvelope, selected bool) string {
	ts := localTime(msg.CreatedAt)
	state := "open"
	if msg.ConsumedAt != nil {
		state = "done"
	}
	title := firstNonEmpty(messageSubject(msg), messageBody(msg), msg.Kind)
	row := fmt.Sprintf("%-5s %-8s %-5s %-24s %s", ts, msg.Kind, state, truncateDetail(msg.From, 24), truncateDetail(title, 54))
	if selected {
		width := s.body.Width - 4
		if width < 20 {
			width = 20
		}
		return s.theme.ResultSelected().Width(width).Render("> " + row)
	}
	return s.theme.ResultUnselected().Render("  " + row)
}

func (s *MessagesScreen) renderSelected(msg client.MessageEnvelope) string {
	var b strings.Builder
	writeField := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		b.WriteString("  ")
		b.WriteString(s.theme.FieldLabel().Render(label))
		b.WriteString(" ")
		b.WriteString(s.theme.FieldValue().Render(value))
		b.WriteByte('\n')
	}
	writeField("id", msg.ID)
	writeField("from", msg.From)
	writeField("to", msg.To)
	writeField("thread", msg.ThreadID)
	writeField("reply_to", msg.InReplyTo)
	writeField("channel", msg.Channel)
	writeField("created", msg.CreatedAt.Local().Format(time.RFC3339))
	b.WriteString("\n")
	b.WriteString(indent(messageBody(msg), "  "))
	b.WriteByte('\n')
	return b.String()
}

func (s *MessagesScreen) renderFooter() string {
	hints := []string{
		s.keyHint(s.keys.Back),
		s.keyHint(s.keys.ScrollUp),
		s.keyHint(s.keys.ScrollDown),
	}
	if s.mode == messageModeView {
		hints = []string{
			s.keyHint(s.keys.Back),
			s.keyHint(s.keys.NextAction),
			"enter " + s.actionLabel(),
			s.keyHint(s.keys.ScrollUp),
			s.keyHint(s.keys.ScrollDown),
		}
	}
	return s.theme.Frame().Render(s.theme.Footer().Render(strings.Join(hints, "  ·  ")))
}

func (s *MessagesScreen) keyHint(k key.Binding) string {
	hk, desc := k.Help().Key, k.Help().Desc
	return s.theme.FooterKey().Render(hk) + " " + desc
}

func (s *MessagesScreen) mergeMessages(rows []client.MessageEnvelope) int {
	seen := make(map[string]struct{}, len(s.messages)+len(rows))
	for _, msg := range s.messages {
		seen[msg.ID] = struct{}{}
	}
	added := 0
	for _, msg := range rows {
		if msg.ID == "" {
			continue
		}
		if _, ok := seen[msg.ID]; ok {
			continue
		}
		seen[msg.ID] = struct{}{}
		s.messages = append(s.messages, msg)
		added++
	}
	sort.SliceStable(s.messages, func(i, j int) bool {
		return s.messages[i].CreatedAt.After(s.messages[j].CreatedAt)
	})
	if s.selected >= len(s.messages) {
		s.selected = len(s.messages) - 1
	}
	if s.selected < 0 {
		s.selected = 0
	}
	return added
}

func (s *MessagesScreen) markConsumed(id string) {
	for i := range s.messages {
		if s.messages[i].ID == id {
			now := time.Now()
			s.messages[i].ConsumedAt = &now
			return
		}
	}
}

func (s *MessagesScreen) selectedMessage() *client.MessageEnvelope {
	if len(s.messages) == 0 || s.selected < 0 || s.selected >= len(s.messages) {
		return nil
	}
	return &s.messages[s.selected]
}

func (s *MessagesScreen) moveSelection(delta int) {
	if len(s.messages) == 0 {
		return
	}
	s.selected += delta
	if s.selected < 0 {
		s.selected = 0
	}
	if s.selected >= len(s.messages) {
		s.selected = len(s.messages) - 1
	}
}

func (s *MessagesScreen) cycleAction(delta int) {
	actions := []messageAction{
		messageActionRead,
		messageActionReply,
		messageActionRefresh,
		messageActionAddress,
		messageActionConsume,
	}
	idx := 0
	for i, action := range actions {
		if action == s.action {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(actions)) % len(actions)
	s.action = actions[idx]
}

func (s *MessagesScreen) runAction() (screen.Screen, tea.Cmd) {
	switch s.action {
	case messageActionRead:
		if s.selectedMessage() == nil {
			return s, nil
		}
		s.mode = messageModeRead
		s.body.SetYOffset(0)
		s.refreshBody()
		return s, nil
	case messageActionRefresh:
		if s.client == nil {
			return s, nil
		}
		s.loading = true
		s.status = "Loading inbox..."
		s.refreshBody()
		return s, loadMuxMessagesCmd(s.client, s.inboxAddress)
	case messageActionAddress:
		s.mode = messageModeAddress
		s.addressInput.SetValue(s.inboxAddress)
		s.addressInput.Focus()
		s.refreshBody()
		return s, textinput.Blink
	case messageActionConsume:
		msg := s.selectedMessage()
		if msg == nil || s.client == nil {
			return s, nil
		}
		s.status = "Marking consumed..."
		s.refreshBody()
		return s, consumeMuxMessageCmd(s.client, msg.ID, s.inboxAddress)
	default:
		if s.selectedMessage() == nil {
			return s, nil
		}
		s.mode = messageModeReply
		s.replyInput.SetValue("")
		s.replyInput.Focus()
		s.refreshBody()
		return s, textinput.Blink
	}
}

func (s *MessagesScreen) actionLabel() string {
	switch s.action {
	case messageActionRead:
		return "read"
	case messageActionRefresh:
		return "refresh"
	case messageActionAddress:
		return "address"
	case messageActionConsume:
		return "consume"
	default:
		return "reply"
	}
}

func (s *MessagesScreen) ensureSelectedVisible() {
	if s.mode != messageModeView || len(s.messages) == 0 {
		return
	}
	top := s.body.YOffset
	bottom := top + s.body.Height - 1
	switch {
	case s.selected < top:
		s.body.SetYOffset(s.selected)
	case s.selected > bottom:
		s.body.SetYOffset(s.selected - s.body.Height + 1)
	}
}

type muxMessagesLoadedMsg struct {
	messages []client.MessageEnvelope
	err      error
}

type muxMessageActionMsg struct {
	status     string
	consumedID string
	err        error
}

func loadMuxMessagesCmd(c *client.Client, address string) tea.Cmd {
	return func() tea.Msg {
		rows, err := c.MessageInbox(context.Background(), address)
		return muxMessagesLoadedMsg{messages: rows, err: err}
	}
}

func consumeMuxMessageCmd(c *client.Client, id, as string) tea.Cmd {
	return func() tea.Msg {
		if err := c.MessageConsume(context.Background(), id, as); err != nil {
			return muxMessageActionMsg{err: err}
		}
		return muxMessageActionMsg{status: "Message consumed.", consumedID: id}
	}
}

func replyToMuxMessageCmd(c *client.Client, from string, msg client.MessageEnvelope, text string) tea.Cmd {
	return func() tea.Msg {
		kind := "notice"
		if msg.Kind == "request" {
			kind = "response"
		}
		threadID := msg.ThreadID
		if threadID == "" {
			threadID = msg.ID
		}
		_, err := c.MessageSend(context.Background(), client.MessageSendRequest{
			Kind:      kind,
			Channel:   firstNonEmpty(msg.Channel, "inbox"),
			From:      from,
			To:        msg.From,
			ThreadID:  threadID,
			InReplyTo: msg.ID,
			Payload: map[string]any{
				"body": text,
			},
			Metadata: map[string]any{
				"source": "tui",
			},
		})
		if err != nil {
			return muxMessageActionMsg{err: err}
		}
		if err := c.MessageConsume(context.Background(), msg.ID, from); err != nil {
			return muxMessageActionMsg{err: err}
		}
		return muxMessageActionMsg{status: "Reply sent.", consumedID: msg.ID}
	}
}

func messageBody(msg client.MessageEnvelope) string {
	if len(msg.Payload) == 0 || string(msg.Payload) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(msg.Payload, &obj); err == nil {
		for _, key := range []string{"body", "content", "text", "summary", "message"} {
			if v, ok := obj[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, msg.Payload); err == nil {
		return compact.String()
	}
	return string(msg.Payload)
}

func messageSubject(msg client.MessageEnvelope) string {
	var obj map[string]any
	if err := json.Unmarshal(msg.Payload, &obj); err == nil {
		if s, ok := obj["subject"].(string); ok {
			return s
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func localTime(t time.Time) string {
	if t.IsZero() {
		return "--:--"
	}
	return t.Local().Format("15:04")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func truncateDetail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "..."
}
