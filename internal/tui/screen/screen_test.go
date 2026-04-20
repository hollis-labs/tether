package screen

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// stubScreen is a minimal Screen used for stack tests.
type stubScreen struct {
	title string
}

func (s stubScreen) Init() tea.Cmd                    { return nil }
func (s stubScreen) Update(tea.Msg) (Screen, tea.Cmd) { return s, nil }
func (s stubScreen) View() string                     { return s.title + " view" }
func (s stubScreen) KeyBindings() []key.Binding       { return nil }
func (s stubScreen) Title() string                    { return s.title }

func TestStackStartsWithInitial(t *testing.T) {
	s := NewStack(stubScreen{title: "root"})
	if s.Len() != 1 {
		t.Fatalf("expected len=1, got %d", s.Len())
	}
	if s.Top().Title() != "root" {
		t.Fatalf("expected root on top, got %s", s.Top().Title())
	}
}

func TestStackPushThenTopReturnsPushed(t *testing.T) {
	s := NewStack(stubScreen{title: "root"})
	s.Push(stubScreen{title: "detail"})
	if s.Len() != 2 {
		t.Fatalf("expected len=2, got %d", s.Len())
	}
	if s.Top().Title() != "detail" {
		t.Fatalf("expected detail on top, got %s", s.Top().Title())
	}
}

func TestStackPopReturnsTopAndShrinks(t *testing.T) {
	s := NewStack(stubScreen{title: "root"})
	s.Push(stubScreen{title: "detail"})
	popped := s.Pop()
	if popped == nil {
		t.Fatal("expected popped screen, got nil")
	}
	if popped.Title() != "detail" {
		t.Fatalf("expected detail popped, got %s", popped.Title())
	}
	if s.Len() != 1 {
		t.Fatalf("expected len=1 after pop, got %d", s.Len())
	}
	if s.Top().Title() != "root" {
		t.Fatalf("expected root after pop, got %s", s.Top().Title())
	}
}

func TestStackPopRefusesToEmpty(t *testing.T) {
	s := NewStack(stubScreen{title: "root"})
	popped := s.Pop()
	if popped != nil {
		t.Fatalf("expected nil pop on single-element stack, got %v", popped)
	}
	if s.Len() != 1 {
		t.Fatalf("expected len=1 preserved, got %d", s.Len())
	}
}

func TestStackReplaceSwapsTop(t *testing.T) {
	s := NewStack(stubScreen{title: "root"})
	s.Push(stubScreen{title: "original"})
	s.Replace(stubScreen{title: "replaced"})
	if s.Top().Title() != "replaced" {
		t.Fatalf("expected replaced on top, got %s", s.Top().Title())
	}
	if s.Len() != 2 {
		t.Fatalf("expected len=2, got %d", s.Len())
	}
}

func TestStackTitlesBottomToTop(t *testing.T) {
	s := NewStack(stubScreen{title: "main"})
	s.Push(stubScreen{title: "detail"})
	s.Push(stubScreen{title: "attach"})
	got := s.Titles()
	want := []string{"main", "detail", "attach"}
	if len(got) != len(want) {
		t.Fatalf("expected %d titles, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("titles[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestPushCmdEmitsPushScreenMsg(t *testing.T) {
	cmd := Push(stubScreen{title: "new"})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from Push")
	}
	msg := cmd()
	p, ok := msg.(PushScreenMsg)
	if !ok {
		t.Fatalf("expected PushScreenMsg, got %T", msg)
	}
	if p.Screen.Title() != "new" {
		t.Fatalf("expected title new, got %s", p.Screen.Title())
	}
}

func TestPopCmdEmitsPopScreenMsg(t *testing.T) {
	cmd := Pop()
	if cmd == nil {
		t.Fatal("expected non-nil cmd from Pop")
	}
	if _, ok := cmd().(PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
}
