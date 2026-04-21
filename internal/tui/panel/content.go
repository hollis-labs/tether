// Package panel provides the side-panel Model and its Content types.
// The panel lives at the TUI root level — a sibling of the screen
// stack — so every screen can drive it without importing detail/*.
package panel

import tea "github.com/charmbracelet/bubbletea"

// ContentKind identifies which renderer a Content value uses.
type ContentKind string

const (
	KindYesNo       ContentKind = "yes_no"
	KindMultiChoice ContentKind = "multi_choice"
	KindTextInput   ContentKind = "text_input"
	KindViewport    ContentKind = "viewport"
)

// Slot distinguishes the two panel content slots.
type Slot int

const (
	SlotEphemeral  Slot = iota // agent-pushed forms and review cards
	SlotPersistent             // user-pinned files and documents
)

// Content is anything the panel can render and optionally interact with.
// Both the ephemeral and persistent slots hold one Content at a time.
type Content interface {
	// View renders the content into width×height cells.
	View(width, height int) string
	// Update handles a key or window-size message routed from the panel.
	// Returns the updated Content and any command to run.
	Update(msg tea.Msg) (Content, tea.Cmd)
	// Kind returns the content's variant; used by the panel header.
	Kind() ContentKind
	// DefaultValue returns the pre-set response string.
	// Used by accept-all to confirm without user interaction.
	// Returns "" for content types that have no meaningful default
	// (e.g., KindViewport).
	DefaultValue() string
	// ToolUseID returns the claude tool_use block ID this content is
	// responding to, or "" if the content is not tied to a tool call
	// (e.g., pinned files).
	ToolUseID() string
}
