package detail

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/layout"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// attachBufCap caps the history we hold in memory. Long-running
// sessions can emit gigabytes; we trim to the last attachBufCap bytes
// so the viewport stays responsive and memory bounded.
const attachBufCap = 64 * 1024

// attachMsg is the union type sent from the byte-stream goroutine to
// the Bubble Tea Update loop. Exactly one of bytes / err / done is
// meaningful per message.
type attachMsg struct {
	bytes []byte
	err   error
	done  bool
}

type attachSendErrMsg struct{ err error }

// attachResizeResultMsg surfaces the outcome of a fire-and-forget
// resize. Non-nil err is displayed as a status-line suffix but never
// detaches the stream.
type attachResizeResultMsg struct{ err error }

// AttachScreen is the live-attach pane. On Init it spawns a goroutine
// that reads from the daemon's attach endpoint into a buffered
// channel; each batch arrives on Update as attachMsg. Keys:
//
//   - Esc            → detach + pop (session keeps running)
//   - Ctrl-C         → forward \x03 (SIGINT) to session
//   - Ctrl-D         → forward \x04 (EOF) to session
//   - Enter          → send input-line + "\n"
//   - anything else  → textinput
type AttachScreen struct {
	theme  theme.Theme
	client *client.Client
	s      api.SessionDTO

	width  int
	height int

	input  textinput.Model
	output viewport.Model

	buf []byte

	ctx    context.Context
	cancel context.CancelFunc
	ch     chan attachMsg

	status   string
	finalErr error
	finished bool

	keys attachKeys
}

type attachKeys struct {
	Detach  key.Binding
	SigInt  key.Binding
	SendEOF key.Binding
	Send    key.Binding
	Palette key.Binding
}

func defaultAttachKeys() attachKeys {
	return attachKeys{
		Detach:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "detach")),
		SigInt:  key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "SIGINT")),
		SendEOF: key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "EOF")),
		Send:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "send")),
		Palette: key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "palette")),
	}
}

// NewAttachScreen constructs an AttachScreen for the given session.
func NewAttachScreen(s api.SessionDTO, c *client.Client) *AttachScreen {
	ti := textinput.New()
	ti.Placeholder = "type input, Enter to send…"
	ti.Prompt = "› "
	ti.Focus()

	vp := viewport.New(0, 0)
	vp.SetContent("Connecting…\n")

	return &AttachScreen{
		theme:  theme.Default(),
		client: c,
		s:      s,
		input:  ti,
		output: vp,
		keys:   defaultAttachKeys(),
	}
}

func (s *AttachScreen) Init() tea.Cmd {
	if s.client == nil {
		s.finished = true
		s.finalErr = fmt.Errorf("no daemon client; attach unavailable in test mode")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.ch = make(chan attachMsg, 32)

	go s.runStream()

	return tea.Batch(textinput.Blink, drainAttachCmd(s.ch))
}

// runStream is the background goroutine that pipes daemon-attach
// bytes into s.ch. Always sends a terminal attachMsg{done: true} so
// the Update loop knows to stop draining.
func (s *AttachScreen) runStream() {
	w := &chanWriter{ch: s.ch, ctx: s.ctx}
	err := s.client.AttachStream(s.ctx, s.s.ID, w)
	// Emit terminal message, honoring cancellation.
	select {
	case s.ch <- attachMsg{err: err, done: true}:
	case <-s.ctx.Done():
	}
}

func (s *AttachScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.resize()
		s.refresh()
		// Propagate the body-viewport's inner dimensions to the
		// session's PTY so full-screen TUI providers (claude CLI,
		// vim) redraw at the right shape. See ADR 0014.
		if s.client != nil && s.output.Width > 0 && s.output.Height > 0 {
			return s, s.resizeSessionCmd(clampDim(s.output.Height), clampDim(s.output.Width))
		}
		return s, nil

	case attachResizeResultMsg:
		if msg.err != nil {
			s.status = "resize failed: " + msg.err.Error()
		} else {
			// Clear any prior resize-failure status on a successful
			// subsequent resize.
			if strings.HasPrefix(s.status, "resize failed: ") {
				s.status = ""
			}
		}
		return s, nil

	case attachMsg:
		if len(msg.bytes) > 0 {
			s.appendBytes(msg.bytes)
		}
		if msg.err != nil {
			s.finalErr = msg.err
		}
		if msg.done {
			s.finished = true
			s.refresh()
			return s, nil
		}
		s.refresh()
		return s, drainAttachCmd(s.ch)

	case attachSendErrMsg:
		s.status = "send failed: " + msg.err.Error()
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *AttachScreen) handleKey(msg tea.KeyMsg) (screen.Screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Detach):
		if s.cancel != nil {
			s.cancel()
		}
		return s, screen.Pop()

	case key.Matches(msg, s.keys.SigInt):
		return s, s.sendBytesCmd([]byte{0x03})

	case key.Matches(msg, s.keys.SendEOF):
		return s, s.sendBytesCmd([]byte{0x04})

	case key.Matches(msg, s.keys.Send):
		line := s.input.Value()
		s.input.SetValue("")
		return s, s.sendBytesCmd([]byte(line + "\n"))
	}

	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return s, cmd
}

func (s *AttachScreen) sendBytesCmd(b []byte) tea.Cmd {
	if s.client == nil {
		return nil
	}
	id := s.s.ID
	c := s.client
	return func() tea.Msg {
		if err := c.SendInput(context.Background(), id, b); err != nil {
			return attachSendErrMsg{err: err}
		}
		return nil
	}
}

// resizeSessionCmd returns a tea.Cmd that posts the current winsize
// to the session's PTY. Fire-and-forget from the Update loop — the
// result comes back as attachResizeResultMsg for status reporting.
func (s *AttachScreen) resizeSessionCmd(rows, cols uint16) tea.Cmd {
	if s.client == nil {
		return nil
	}
	id := s.s.ID
	c := s.client
	return func() tea.Msg {
		err := c.ResizeSession(context.Background(), id, rows, cols)
		return attachResizeResultMsg{err: err}
	}
}

func (s *AttachScreen) appendBytes(p []byte) {
	s.buf = append(s.buf, p...)
	if len(s.buf) > attachBufCap {
		drop := len(s.buf) - attachBufCap
		s.buf = s.buf[drop:]
	}
}

func (s *AttachScreen) resize() {
	const verticalOverhead = 1 + 3 + 1 + 2 // header + input (bordered 3) + footer + body border
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

func (s *AttachScreen) refresh() {
	content := string(s.buf)
	if s.finished {
		suffix := "\n\n[detached]"
		if s.finalErr != nil && !isBenignDetachErr(s.finalErr) {
			suffix = "\n\n[stream closed: " + s.finalErr.Error() + "]"
		}
		content += suffix
	}
	s.output.SetContent(content)
	// Keep the viewport pinned to the bottom so new output stays
	// visible.
	s.output.GotoBottom()
}

func (s *AttachScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	shortID := s.s.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	header := s.theme.Header().Width(s.width - 2).
		Render(fmt.Sprintf("Attach %s · %s · state=%s", shortID, s.s.Workspace, s.s.State))

	bodyFrame := s.theme.Body().Width(s.width - 2).Render(s.output.View())
	inputFrame := s.theme.Search().Width(s.width - 2).Render(s.input.View())
	mid := lipgloss.JoinVertical(lipgloss.Left, bodyFrame, inputFrame)

	hints := []string{
		s.keyHint(s.keys.Detach),
		s.keyHint(s.keys.SigInt),
		s.keyHint(s.keys.SendEOF),
		s.keyHint(s.keys.Send),
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

func (s *AttachScreen) keyHint(k key.Binding) string {
	hk, desc := k.Help().Key, k.Help().Desc
	return s.theme.FooterKey().Render(hk) + " " + desc
}

func (s *AttachScreen) KeyBindings() []key.Binding {
	return []key.Binding{s.keys.Detach, s.keys.SigInt, s.keys.SendEOF, s.keys.Send, s.keys.Palette}
}

func (s *AttachScreen) Title() string {
	short := s.s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return "Attach " + short
}

// drainAttachCmd returns a tea.Cmd that blocks on ch until one
// attachMsg arrives, then returns it. On channel close returns a
// terminal done msg so the screen knows to stop draining.
func drainAttachCmd(ch <-chan attachMsg) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-ch
		if !ok {
			return attachMsg{done: true}
		}
		return m
	}
}

// chanWriter is an io.Writer that forwards each Write as an attachMsg
// onto ch. Writes block until either ch accepts the message or ctx
// is canceled (detach); the latter returns ctx.Err so the upstream
// io.Copy in client.AttachSession unwinds cleanly.
type chanWriter struct {
	ch  chan<- attachMsg
	ctx context.Context
}

func (w *chanWriter) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case w.ch <- attachMsg{bytes: cp}:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

// clampDim safely narrows an int terminal-dimension value to the
// uint16 shape the daemon resize endpoint accepts. Negative and zero
// values clamp to 1 (zero would be rejected by the handler); values
// above math.MaxUint16 clamp to MaxUint16.
func clampDim(n int) uint16 {
	const maxU16 = int(^uint16(0))
	if n < 1 {
		return 1
	}
	if n > maxU16 {
		return uint16(maxU16)
	}
	return uint16(n)
}

// isBenignDetachErr classifies context-cancel / EOF errors as
// expected consequences of the user detaching — no need to surface
// them as error banners.
func isBenignDetachErr(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "context canceled") ||
		strings.Contains(s, "EOF")
}
