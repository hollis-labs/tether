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
	Quit key.Binding
	Help key.Binding
}

// DefaultKeyMap returns the scaffold bindings: q / Ctrl-C quit; ? help.
// The ? overlay itself lands in Sprint 6, but shipping the binding now
// lets the footer advertise it consistently from T-02 onward.
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
	}
}
