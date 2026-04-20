package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// placeholderScreen is a minimal Screen used to verify the root's
// push/pop plumbing without pulling in real detail screens.
type placeholderScreen struct {
	title  string
	width  int
	height int
}

func (p placeholderScreen) Init() tea.Cmd { return nil }
func (p placeholderScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	if m, ok := msg.(tea.WindowSizeMsg); ok {
		p.width, p.height = m.Width, m.Height
	}
	return p, nil
}
func (p placeholderScreen) View() string               { return p.title }
func (p placeholderScreen) KeyBindings() []key.Binding { return nil }
func (p placeholderScreen) Title() string              { return p.title }

func TestRootPushesScreenOnPushMsg(t *testing.T) {
	m := New(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)

	pushed := placeholderScreen{title: "detail"}
	next, _ = m.Update(screen.PushScreenMsg{Screen: pushed})
	m = next.(Model)

	if m.stack.Len() != 2 {
		t.Fatalf("expected stack len=2 after push, got %d", m.stack.Len())
	}
	if m.stack.Top().Title() != "detail" {
		t.Fatalf("expected detail on top, got %s", m.stack.Top().Title())
	}
}

func TestRootForwardsWindowSizeToPushedScreen(t *testing.T) {
	m := New(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(Model)

	pushed := placeholderScreen{title: "detail"}
	next, _ = m.Update(screen.PushScreenMsg{Screen: pushed})
	m = next.(Model)

	top, ok := m.stack.Top().(placeholderScreen)
	if !ok {
		t.Fatalf("expected placeholderScreen on top, got %T", m.stack.Top())
	}
	if top.width != 100 || top.height != 40 {
		t.Fatalf("expected pushed screen sized to 100x40, got %dx%d", top.width, top.height)
	}
}

func TestRootPopReturnsToMain(t *testing.T) {
	m := New(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)

	next, _ = m.Update(screen.PushScreenMsg{Screen: placeholderScreen{title: "detail"}})
	m = next.(Model)

	next, _ = m.Update(screen.PopScreenMsg{})
	m = next.(Model)

	if m.stack.Len() != 1 {
		t.Fatalf("expected stack len=1 after pop, got %d", m.stack.Len())
	}
	if m.stack.Top().Title() != "Main" {
		t.Fatalf("expected Main on top after pop, got %s", m.stack.Top().Title())
	}
}

func TestRootPopIsNoOpOnMainAlone(t *testing.T) {
	m := New(nil)
	next, _ := m.Update(screen.PopScreenMsg{})
	m = next.(Model)
	if m.stack.Len() != 1 {
		t.Fatalf("expected len=1 preserved, got %d", m.stack.Len())
	}
	if m.stack.Top().Title() != "Main" {
		t.Fatalf("expected Main still on top, got %s", m.stack.Top().Title())
	}
}
