// Package tui is the Bubble Tea-based interactive surface for Agent Mux.
// The package is organized around a root Model that composes sub-models
// (search, results, footer, toasts) introduced across v0.0.3 Sprints 1-6.
//
// T-01 (this file set) ships the scaffold: program wiring, a minimal
// root model that honors quit keys, file-based logging, and a shared
// KeyMap the footer and future ? overlay will both read from.
package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap groups the key bindings used across the TUI. Sub-features add
// their own maps; this root map holds the truly-global shortcuts that
// apply regardless of the current screen. The footer (T-02) and the ?
// help overlay (Sprint 6) both enumerate this table so rendered hints
// and actual bindings stay in lockstep.
type KeyMap struct {
	Quit            key.Binding
	Help            key.Binding
	OpenDetail      key.Binding
	Refresh         key.Binding
	FocusSearch     key.Binding
	BlurSearch      key.Binding
	CycleChip       key.Binding
	CycleChipBack   key.Binding
	ToggleProjects  key.Binding
	ToggleAgents    key.Binding
	ToggleProviders key.Binding
	ToggleLaunches  key.Binding
	ToggleSessions  key.Binding
}

// DefaultKeyMap returns the scaffold bindings.
//
// Design notes:
//   - Ctrl-C is the authoritative quit — always works, never collides.
//   - `q` quits only when the search input is blurred (otherwise it
//     types 'q' into the field). Esc blurs; `/` re-focuses.
//   - Tab cycles the chip row through "solo-filter" states:
//     all-on → projects → agents → providers → launches → sessions →
//     all-on. Shift+Tab reverses. Universal across terminals
//     (unlike Alt+N which macOS doesn't forward without Meta-key config).
//   - Alt+1..5 remain as per-filter direct toggles for users who want
//     to compose combinations (e.g., projects + launches only).
//   - Navigation keys (arrows, pgup/pgdn) always scroll the viewport
//     regardless of focus.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		OpenDetail: key.NewBinding(
			key.WithKeys("right"),
			key.WithHelp("→", "detail"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "refresh"),
		),
		FocusSearch: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		BlurSearch: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "blur"),
		),
		CycleChip: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "cycle filter"),
		),
		CycleChipBack: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("⇧tab", "cycle back"),
		),
		ToggleProjects: key.NewBinding(
			key.WithKeys("alt+1"),
			key.WithHelp("⌥1", "projects"),
		),
		ToggleAgents: key.NewBinding(
			key.WithKeys("alt+2"),
			key.WithHelp("⌥2", "agents"),
		),
		ToggleProviders: key.NewBinding(
			key.WithKeys("alt+3"),
			key.WithHelp("⌥3", "providers"),
		),
		ToggleLaunches: key.NewBinding(
			key.WithKeys("alt+4"),
			key.WithHelp("⌥4", "launches"),
		),
		ToggleSessions: key.NewBinding(
			key.WithKeys("alt+5"),
			key.WithHelp("⌥5", "sessions"),
		),
	}
}
