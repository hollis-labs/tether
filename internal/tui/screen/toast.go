package screen

import tea "github.com/charmbracelet/bubbletea"

// ToastKind distinguishes info-style toasts from error-style toasts
// on the cross-screen surface. MainScreen maps these onto its own
// internal toast queue when it receives a ToastEmitMsg.
type ToastKind int

const (
	ToastInfo ToastKind = iota
	ToastError
)

// ToastEmitMsg is emitted (via the Toast helper) by any screen that
// wants to surface a banner on the main screen — e.g., a detail
// screen reporting a successful session stop after it pops itself.
type ToastEmitMsg struct {
	Kind ToastKind
	Text string
}

// Toast returns a tea.Cmd that emits a ToastEmitMsg. Screens call it
// directly; the root Model / MainScreen consumes the msg.
func Toast(kind ToastKind, text string) tea.Cmd {
	return func() tea.Msg { return ToastEmitMsg{Kind: kind, Text: text} }
}
