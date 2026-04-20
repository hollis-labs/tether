package detail

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

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
}

func NewSessionScreen(s api.SessionDTO, c *client.Client) SessionScreen {
	short := s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	attachKey := key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "attach"))
	b := newBase("Session " + short)
	b.extra = append(b.extra, attachKey)
	return SessionScreen{base: b, s: s, client: c, attachKey: attachKey}
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
	if m, ok := msg.(sessionRefetchedMsg); ok {
		if m.err != nil {
			s.err = m.err
		} else {
			s.s = m.dto
		}
		s.refresh()
		return s, nil
	}
	if km, ok := msg.(tea.KeyMsg); ok {
		if key.Matches(km, s.attachKey) && s.client != nil {
			return s, screen.Push(NewAttachScreen(s.s, s.client))
		}
	}
	cmd, handled := s.updateCommon(msg)
	if handled {
		s.refresh()
		return s, cmd
	}
	return s, nil
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
