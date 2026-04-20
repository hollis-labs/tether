// Package screen defines the Screen interface + Stack used to compose
// the multi-screen TUI introduced in v0.0.3 Sprint 2.
//
// Design contract:
//
//   - Every visible pane is a Screen. The main results view, each
//     detail pane, the command palette, and the live-attach surface
//     all satisfy the interface.
//   - Screens live on a Stack. Only the top screen is rendered and
//     receives input in v0.0.3 (no peek-through overlays; Sprint 6 or
//     later may relax this if the need appears).
//   - Key handling is screen-local. Each screen decides what keys to
//     consume — including Ctrl-C. The attach screen forwards Ctrl-C to
//     its session as a SIGINT byte; the main screen quits. The root
//     model has no global Ctrl-C fallback so screens have full control.
//   - Navigation happens via PushScreenMsg and PopScreenMsg. Screens
//     never touch the Stack directly; they emit these messages from
//     their Update via tea.Cmd. The root model intercepts the messages
//     and mutates the Stack.
package screen

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// Screen is the contract every TUI pane implements. Value-receiver
// types are the Bubble Tea idiom; Update returns a fresh Screen so
// the interface wrapper can carry the new state.
type Screen interface {
	// Init returns the command(s) to run when the screen is pushed.
	// Invoked once by the root on push.
	Init() tea.Cmd

	// Update consumes a message and returns the updated screen plus
	// any command to run. Returning a different Screen type is
	// allowed when a screen wants to mutate into another variant
	// without going through Push/Pop (rare).
	Update(tea.Msg) (Screen, tea.Cmd)

	// View returns the full-frame rendering at whatever size the
	// screen last received via tea.WindowSizeMsg.
	View() string

	// KeyBindings returns the screen-local bindings, used by the
	// root's footer (to render the hint line for the top screen) and
	// by Sprint 6's `?` overlay.
	KeyBindings() []key.Binding

	// Title returns a short label for the breadcrumb header —
	// e.g. "Main", "Session abc123", "Attach abc123".
	Title() string
}

// Stack is a LIFO collection of Screens. The zero value is unusable;
// construct with NewStack(initial).
//
// Stack is not safe for concurrent use. The Bubble Tea Update loop
// serializes all access, so callers never need to lock.
type Stack struct {
	screens []Screen
}

// NewStack returns a Stack with initial as its bottom (and currently
// top) element. The stack's invariant is Len() >= 1 at all times;
// Pop refuses to remove the last screen.
func NewStack(initial Screen) *Stack {
	return &Stack{screens: []Screen{initial}}
}

// Push adds s to the top of the stack.
func (s *Stack) Push(next Screen) {
	s.screens = append(s.screens, next)
}

// Pop removes and returns the top screen. Returns nil and does not
// mutate the stack if only one screen remains — the root can't
// accidentally empty the stack via Esc on the main screen.
func (s *Stack) Pop() Screen {
	if len(s.screens) <= 1 {
		return nil
	}
	top := s.screens[len(s.screens)-1]
	s.screens = s.screens[:len(s.screens)-1]
	return top
}

// Top returns the current top screen. Panics if the stack is empty —
// which should never happen because NewStack requires an initial
// screen and Pop refuses to drop below one.
func (s *Stack) Top() Screen {
	if len(s.screens) == 0 {
		panic("screen stack is empty; NewStack must be called with an initial screen")
	}
	return s.screens[len(s.screens)-1]
}

// Replace swaps the top screen in place. Used by the root's Update
// to persist the result of stack.Top().Update.
func (s *Stack) Replace(top Screen) {
	if len(s.screens) == 0 {
		return
	}
	s.screens[len(s.screens)-1] = top
}

// Len returns the number of screens currently on the stack.
func (s *Stack) Len() int {
	return len(s.screens)
}

// Titles returns the titles of every screen bottom-to-top. Rendered
// as a breadcrumb by the root: "Main › Session abc123".
func (s *Stack) Titles() []string {
	out := make([]string, len(s.screens))
	for i, sc := range s.screens {
		out[i] = sc.Title()
	}
	return out
}

// PushScreenMsg is the tea.Msg a screen emits (via a tea.Cmd) to
// request that `Screen` be pushed on top.
type PushScreenMsg struct {
	Screen Screen
}

// PopScreenMsg is the tea.Msg a screen emits (via a tea.Cmd) to pop
// itself. The root ignores it when only the main screen remains.
type PopScreenMsg struct{}

// Push returns a tea.Cmd that emits a PushScreenMsg carrying s.
// Convenience so screen code reads as:
//
//	return m, screen.Push(detail.NewProjectScreen(...))
func Push(s Screen) tea.Cmd {
	return func() tea.Msg { return PushScreenMsg{Screen: s} }
}

// Pop returns a tea.Cmd that emits a PopScreenMsg.
func Pop() tea.Cmd {
	return func() tea.Msg { return PopScreenMsg{} }
}
