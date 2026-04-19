package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRootModelQuitOnCtrlC(t *testing.T) {
	m := New()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected a quit command on Ctrl-C, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
}

func TestRootModelQuitOnQ(t *testing.T) {
	m := New()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected a quit command on 'q', got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
}

func TestRootModelIgnoresUnknownKey(t *testing.T) {
	m := New()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd != nil {
		t.Fatalf("expected no command for unbound key, got %T", cmd())
	}
}

func TestRootModelTracksResize(t *testing.T) {
	m := New()
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if cmd != nil {
		t.Fatalf("expected no command on resize, got %T", cmd())
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected tui.Model, got %T", next)
	}
	if nm.width != 100 || nm.height != 40 {
		t.Fatalf("expected 100x40, got %dx%d", nm.width, nm.height)
	}
}
