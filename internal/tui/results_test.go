package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
)

func newSeededModel(t *testing.T) MainScreen {
	t.Helper()
	m := NewMainScreen(nil)
	// Give it a reasonable rendering budget.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(MainScreen)

	// Prime with fixture data; simulate all fanned-in loads (one per chipOrder entry).
	loads := []catalogLoadedMsg{
		{typ: RowTypeProjects, rows: rowsFromProjects([]config.Project{
			{ID: "acme", Name: "Acme Corp"},
			{ID: "beta", Name: "Beta Project"},
		})},
		{typ: RowTypeAgents, rows: rowsFromAgents([]config.Agent{
			{ID: "writer", Name: "Writer Agent"},
		})},
		{typ: RowTypeProviders, rows: rowsFromProviders([]config.Provider{
			{ID: "stub", Type: "api"},
			{ID: "claudecode", Type: "cli"},
		})},
		{typ: RowTypeLaunches, rows: rowsFromLaunches([]config.Launch{
			{ID: "demo-launch", Project: "acme", Agent: "writer", Provider: "stub"},
		})},
		{typ: RowTypeBootProfiles, rows: nil}, // no boot profiles in this fixture
		{typ: RowTypeSessions, rows: rowsFromSessions([]api.SessionDTO{
			{ID: "01234567-aaaa-bbbb-cccc-000000000000", State: "running", ProjectID: "acme", LogicalAgentID: "writer"},
		})},
		{typ: RowTypeLogicalAgents, rows: rowsFromLogicalAgents([]api.LogicalAgentSummary{})},
	}
	for _, msg := range loads {
		next, _ := m.Update(msg)
		m = next.(MainScreen)
	}
	return m
}

func TestCatalogLoadedPopulatesVisibleRows(t *testing.T) {
	m := newSeededModel(t)
	if len(m.visible) != 7 {
		t.Fatalf("expected 7 visible rows (2+1+2+1+1), got %d", len(m.visible))
	}
	if m.loadRemaining != 0 {
		t.Fatalf("expected loadRemaining=0 after all chip loads, got %d", m.loadRemaining)
	}
}

func TestCatalogLoadErrorRecorded(t *testing.T) {
	m := NewMainScreen(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(MainScreen)
	next, _ = m.Update(catalogLoadedMsg{typ: RowTypeProjects, err: errors.New("boom")})
	m = next.(MainScreen)
	if len(m.loadErrs) != 1 {
		t.Fatalf("expected 1 load error, got %d", len(m.loadErrs))
	}
}

func TestChipToggleHidesRowType(t *testing.T) {
	m := newSeededModel(t)
	// Disable providers via Alt+3.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}, Alt: true})
	m = next.(MainScreen)
	// After toggle, 2 provider rows should be gone → 5 visible.
	if len(m.visible) != 5 {
		t.Fatalf("expected 5 visible after providers off, got %d", len(m.visible))
	}
	for _, r := range m.visible {
		if r.Type() == RowTypeProviders {
			t.Fatalf("provider row leaked through disabled chip: %s", r.ID())
		}
	}
}

func TestFuzzyFilterOnSearch(t *testing.T) {
	m := newSeededModel(t)
	// Type "acme" — should match the acme project by title/ID.
	for _, r := range "acme" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(MainScreen)
	}
	if len(m.visible) == 0 {
		t.Fatal("expected at least one result for 'acme'")
	}
	if m.visible[0].Type() != RowTypeProjects && m.visible[0].ID() != "acme" {
		t.Fatalf("expected acme project as top match, got %s/%s", m.visible[0].Type(), m.visible[0].ID())
	}
}

func TestSelectedRowReturnsNilWhenEmpty(t *testing.T) {
	m := NewMainScreen(nil)
	if got := m.SelectedRow(); got != nil {
		t.Fatalf("expected nil SelectedRow on fresh model, got %+v", got)
	}
}

func TestSelectionMovesWithinVisible(t *testing.T) {
	m := newSeededModel(t)
	if m.selectedIdx != 0 {
		t.Fatalf("expected selectedIdx=0 initially, got %d", m.selectedIdx)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(MainScreen)
	if m.selectedIdx != 1 {
		t.Fatalf("expected selectedIdx=1 after Down, got %d", m.selectedIdx)
	}
	// Move up past the top; should clamp.
	for i := 0; i < 5; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(MainScreen)
	}
	if m.selectedIdx != 0 {
		t.Fatalf("expected clamp to 0 at top, got %d", m.selectedIdx)
	}
}

func TestRenderBodyShowsSelectionMarker(t *testing.T) {
	m := newSeededModel(t)
	out := m.renderBody()
	if !strings.Contains(out, "▶") {
		t.Fatalf("expected selection marker in body, got:\n%s", out)
	}
}

func TestEmptyStateRendersOnNoMatches(t *testing.T) {
	m := newSeededModel(t)
	// Type gibberish that matches nothing.
	for _, r := range "zzzzzzz" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(MainScreen)
	}
	if len(m.visible) != 0 {
		t.Fatalf("expected empty visible, got %d", len(m.visible))
	}
	out := m.renderBody()
	if !strings.Contains(out, "No matches") {
		t.Fatalf("expected 'No matches' in body, got:\n%s", out)
	}
}
