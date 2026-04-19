package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRootModelQuitOnCtrlC(t *testing.T) {
	m := New(nil) // search focused by default
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected a quit command on Ctrl-C, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
}

func TestRootModelQDoesNotQuitWhileSearchFocused(t *testing.T) {
	m := New(nil) // search focused by default
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected 'q' to NOT quit while search is focused")
		}
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if nm.search.Value() != "q" {
		t.Fatalf("expected search value 'q', got %q", nm.search.Value())
	}
}

func TestRootModelQQuitsAfterSearchBlur(t *testing.T) {
	m := New(nil)
	// Blur the search input.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.search.Focused() {
		t.Fatal("expected search blurred after Esc")
	}
	// Now 'q' should quit.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected a quit command on 'q' when search is blurred, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", cmd())
	}
}

func TestRootModelSearchRefocusesOnSlash(t *testing.T) {
	m := New(nil)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.search.Focused() {
		t.Fatal("expected blurred after Esc")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(Model)
	if !m.search.Focused() {
		t.Fatal("expected / to re-focus search")
	}
}

func TestRootModelTracksResize(t *testing.T) {
	m := New(nil)
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		t.Fatalf("expected no command on resize, got %T", cmd())
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if nm.width != 120 || nm.height != 40 {
		t.Fatalf("expected 120x40, got %dx%d", nm.width, nm.height)
	}
	if nm.body.Width == 0 || nm.body.Height == 0 {
		t.Fatal("expected viewport to be sized after resize")
	}
}

func TestChipToggleFlipsFilter(t *testing.T) {
	m := New(nil)
	if !m.filters[RowTypeProjects] {
		t.Fatal("expected projects filter enabled by default")
	}
	// Alt+1 toggles projects off.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}, Alt: true})
	m = next.(Model)
	if m.filters[RowTypeProjects] {
		t.Fatal("expected projects filter OFF after Alt+1")
	}
	// Second Alt+1 flips it back.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}, Alt: true})
	m = next.(Model)
	if !m.filters[RowTypeProjects] {
		t.Fatal("expected projects filter ON after second Alt+1")
	}
}

func TestChipToggleAllFiveBindings(t *testing.T) {
	m := New(nil)
	digits := []struct {
		d rune
		t RowType
	}{
		{'1', RowTypeProjects},
		{'2', RowTypeAgents},
		{'3', RowTypeProviders},
		{'4', RowTypeLaunches},
		{'5', RowTypeSessions},
	}
	for _, tc := range digits {
		before := m.filters[tc.t]
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tc.d}, Alt: true})
		m = next.(Model)
		if m.filters[tc.t] == before {
			t.Fatalf("Alt+%c did not toggle %s filter", tc.d, tc.t)
		}
	}
}

func TestSearchCapturesTyping(t *testing.T) {
	m := New(nil)
	for _, r := range "hello" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}
	if m.search.Value() != "hello" {
		t.Fatalf("expected search value 'hello', got %q", m.search.Value())
	}
}
