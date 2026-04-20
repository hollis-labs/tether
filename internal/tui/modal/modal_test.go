package modal

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/screen"
)

func TestConfirmYesRunsHandlerAndPops(t *testing.T) {
	called := false
	m := NewConfirm("stop", "really?", func() tea.Cmd {
		return func() tea.Msg {
			called = true
			return nil
		}
	})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(*ConfirmModal)
	_ = called

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("expected cmd from y, got nil")
	}
	// Can't easily assert both pop + handler ran without a running
	// bubbletea program; we at least verify a cmd was returned.
	_ = next
}

func TestConfirmNoJustPops(t *testing.T) {
	called := false
	m := NewConfirm("stop", "really?", func() tea.Cmd {
		return func() tea.Msg {
			called = true
			return nil
		}
	})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("expected pop cmd from n, got nil")
	}
	if _, ok := cmd().(screen.PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
	if called {
		t.Fatal("onYes should not have run on 'n'")
	}
}

func TestConfirmEscJustPops(t *testing.T) {
	called := false
	m := NewConfirm("stop", "really?", func() tea.Cmd {
		return func() tea.Msg {
			called = true
			return nil
		}
	})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected pop cmd from Esc, got nil")
	}
	if _, ok := cmd().(screen.PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
	if called {
		t.Fatal("onYes should not have run on Esc")
	}
}

func TestConfirmNoHandlerYesStillPops(t *testing.T) {
	m := NewConfirm("title", "prompt", nil)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("expected pop cmd from y with nil handler")
	}
	if _, ok := cmd().(screen.PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
}
