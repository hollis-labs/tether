package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ToastKind distinguishes info toasts from error toasts so styling
// and any future filtering can differ.
type ToastKind int

const (
	ToastInfo ToastKind = iota
	ToastError
)

// toastTTL is how long a toast remains visible before it auto-dismisses.
// Keep conservative: users need time to read the session ID before it
// disappears, but not so long that stale toasts accumulate on the screen.
const toastTTL = 5 * time.Second

// Toast is a transient status message rendered above the footer.
type Toast struct {
	ID      int
	Kind    ToastKind
	Message string
}

// toastExpiredMsg is emitted via tea.Tick when a toast's TTL elapses.
// Update removes the matching toast from the stack.
type toastExpiredMsg struct{ ID int }

// toastQueue owns the active toasts and the monotonically-increasing
// ID counter.
type toastQueue struct {
	toasts []Toast
	nextID int
}

// push adds a toast and returns the tea.Cmd that fires its expiration.
// Callers return the cmd from Update so Bubble Tea schedules the tick.
func (q *toastQueue) push(kind ToastKind, message string) tea.Cmd {
	id := q.nextID
	q.nextID++
	q.toasts = append(q.toasts, Toast{ID: id, Kind: kind, Message: message})
	return tea.Tick(toastTTL, func(time.Time) tea.Msg {
		return toastExpiredMsg{ID: id}
	})
}

// remove drops the toast with the given ID. No-op if not present.
func (q *toastQueue) remove(id int) {
	out := q.toasts[:0]
	for _, t := range q.toasts {
		if t.ID != id {
			out = append(out, t)
		}
	}
	q.toasts = out
}

// items returns a snapshot of the current toast stack for rendering.
func (q *toastQueue) items() []Toast {
	// Return as-is; View-side rendering doesn't mutate.
	return q.toasts
}
