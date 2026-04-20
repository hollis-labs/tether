package detail

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/externshell"
	"github.com/chrispian/agent-mux/internal/tui/modal"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// sessionStoppedMsg carries the result of a stop request back into
// SessionScreen so it can pop + emit a toast via the cross-screen
// toast surface.
type sessionStoppedMsg struct {
	id  string
	err error
}

// sessionRefetchedMsg carries the result of an on-Init GetSession
// re-fetch so the detail screen can show the latest state (the session
// list's rows may be stale if the session transitioned since the last
// list call).
type sessionRefetchedMsg struct {
	dto api.SessionDTO
	err error
}

// SessionScreen renders an api.SessionDTO. On Init it kicks off a
// GetSession re-fetch so the displayed state reflects "right now"
// rather than "whenever the list was loaded."
type SessionScreen struct {
	base
	s         api.SessionDTO
	client    *client.Client
	err       error
	attachKey key.Binding
	openKey   key.Binding
	stopKey   key.Binding
	tailKey   key.Binding
}

func NewSessionScreen(s api.SessionDTO, c *client.Client) SessionScreen {
	short := s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	attachKey := key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "attach"))
	openKey := key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open shell"))
	stopKey := key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop"))
	tailKey := key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tail"))
	b := newBase("Session " + short)
	b.extra = append(b.extra, attachKey, openKey, stopKey, tailKey)
	return SessionScreen{base: b, s: s, client: c,
		attachKey: attachKey, openKey: openKey, stopKey: stopKey, tailKey: tailKey}
}

func (s SessionScreen) Init() tea.Cmd {
	if s.client == nil {
		return nil
	}
	id := s.s.ID
	c := s.client
	return func() tea.Msg {
		dto, err := c.ListSessions(context.Background()) // fallback: list then filter; GetSession exists in inner client but tui client wrapper doesn't re-export it
		if err != nil {
			return sessionRefetchedMsg{err: err}
		}
		for _, d := range dto {
			if d.ID == id {
				return sessionRefetchedMsg{dto: d}
			}
		}
		return sessionRefetchedMsg{err: fmt.Errorf("session %s not found in list response", id)}
	}
}

func (s SessionScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case sessionRefetchedMsg:
		if m.err != nil {
			s.err = m.err
		} else {
			s.s = m.dto
		}
		s.refresh()
		return s, nil

	case sessionStoppedMsg:
		short := m.id
		if len(short) > 8 {
			short = short[:8]
		}
		if m.err != nil {
			return s, screen.Toast(screen.ToastError, "Stop failed: "+m.err.Error())
		}
		// Success: pop session detail back to main, and push toast.
		return s, tea.Batch(
			screen.Pop(),
			screen.Toast(screen.ToastInfo, "Stopped session "+short),
		)

	case tea.KeyMsg:
		if key.Matches(m, s.attachKey) && s.client != nil {
			return s, screen.Push(NewAttachScreen(s.s, s.client))
		}
		if key.Matches(m, s.openKey) {
			return s, openExternalAttachCmd(s.s.ID)
		}
		if key.Matches(m, s.stopKey) && s.client != nil {
			return s, screen.Push(s.newStopConfirm())
		}
		if key.Matches(m, s.tailKey) {
			return s, screen.Push(NewTailScreen(s.s))
		}
	}
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
}

// newStopConfirm constructs the confirmation modal that, on yes,
// fires a stopSessionCmd. The modal pops itself; its onYes-returned
// cmd then posts /stop asynchronously. The resulting
// sessionStoppedMsg routes back here to pop-and-toast.
func (s SessionScreen) newStopConfirm() *modal.ConfirmModal {
	short := s.s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return modal.NewConfirm(
		"Stop session "+short,
		"This terminates the running session. Continue?",
		func() tea.Cmd { return stopSessionCmd(s.client, s.s.ID) },
	)
}

// stopSessionCmd posts /sessions/{id}/stop on a worker goroutine and
// returns a sessionStoppedMsg with the outcome.
func stopSessionCmd(c *client.Client, id string) tea.Cmd {
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		err := c.StopSession(context.Background(), id)
		return sessionStoppedMsg{id: id, err: err}
	}
}

// openExternalAttachCmd spawns a platform terminal running
// `mux sessions attach <id>` via externshell.AttachIn. The result
// surfaces as a screen.ToastEmitMsg so MainScreen reports either
// success ("opened external terminal") or the spawn error.
func openExternalAttachCmd(id string) tea.Cmd {
	return func() tea.Msg {
		if err := externshell.AttachIn(id); err != nil {
			return screen.ToastEmitMsg{
				Kind: screen.ToastError,
				Text: "open shell failed: " + err.Error(),
			}
		}
		return screen.ToastEmitMsg{
			Kind: screen.ToastInfo,
			Text: "opened external terminal (raw attach)",
		}
	}
}

func (s SessionScreen) View() string {
	s.refresh()
	return s.render()
}

func (s SessionScreen) KeyBindings() []key.Binding { return s.bindings() }
func (s SessionScreen) Title() string              { return s.title }

func (s *SessionScreen) refresh() {
	fields := []field{
		{label: "id", value: s.s.ID},
		{label: "state", value: s.s.State},
		{label: "launch id", value: s.s.LaunchID},
		{label: "project", value: s.s.ProjectID},
		{label: "logical agent", value: s.s.LogicalAgentID},
		{label: "provider", value: s.s.ProviderID},
		{label: "workspace", value: s.s.Workspace},
		{label: "pid", value: intPtrStr(s.s.PID)},
		{label: "exit code", value: intPtrStr(s.s.ExitCode)},
		{label: "created at", value: s.s.CreatedAt},
		{label: "updated at", value: s.s.UpdatedAt},
		{label: "ended at", value: strPtr(s.s.EndedAt)},
		{label: "attached clients", value: fmt.Sprintf("%d", s.s.AttachedClients)},
	}
	if s.err != nil {
		fields = append(fields, field{label: "refetch error", value: s.err.Error()})
	}
	s.setContent(renderFields(s.theme, fields))
}

func intPtrStr(p *int) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *p)
}

func strPtr(p *string) string {
	if p == nil {
		return "—"
	}
	return *p
}
