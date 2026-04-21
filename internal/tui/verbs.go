package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/palette"
)

// openPaletteMsg is emitted by any screen that wants to push the
// command palette. The root Model intercepts it and does the push so
// screens don't need a reference to the Registry.
type openPaletteMsg struct{}

// applyFilterMsg is emitted by the palette's "view <type>" verbs.
// MainScreen handles it by setting its filter chips to solo-mode for
// the requested type.
type applyFilterMsg struct {
	solo RowType
}

// panelPinFileMsg is emitted when the user runs "pin file" from the
// palette. Path argument parsing is a follow-up; this registers the
// verb so it appears in palette autocomplete.
type panelPinFileMsg struct{ path string } //nolint:unused

// openPalette is the cmd screens emit when the user presses `:` or
// Ctrl+K. Kept as a helper so every screen's trigger call reads the
// same way.
func openPalette() tea.Cmd {
	return func() tea.Msg { return openPaletteMsg{} }
}

// registerDefaultVerbs wires the Sprint-2 verb set into the given
// registry. Sprints 3+ register additional verbs from their own
// packages without touching this function.
func registerDefaultVerbs(reg *palette.Registry) {
	for _, t := range chipOrder {
		filter := t
		reg.Register(palette.Verb{
			Name:        "view " + string(filter),
			Description: fmt.Sprintf("Show only %s rows on the main screen", filter),
			Handler: func() tea.Cmd {
				return func() tea.Msg { return applyFilterMsg{solo: filter} }
			},
		})
	}
	reg.Register(palette.Verb{
		Name:        "quit",
		Description: "Exit the TUI",
		Aliases:     []string{"exit", "q"},
		Handler:     func() tea.Cmd { return tea.Quit },
	})
	reg.Register(palette.Verb{
		Name:        "pin file",
		Description: "Pin a file to the side panel persistent slot (usage: pin file <path>)",
		Handler: func() tea.Cmd {
			// Path argument parsing is a follow-up item.
			// For now, registering the verb for palette autocomplete.
			return nil
		},
	})
}
