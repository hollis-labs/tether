package panel

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ViewportContent wraps bubbles/viewport for read-only content: review
// cards, diffs, documents, and user-pinned files.
type ViewportContent struct {
	body      string // raw text/ANSI content
	toolUseID string // empty for pinned files; set for review_card/diff
	vp        viewport.Model
	th        theme.Theme
}

// NewViewportContent constructs a ViewportContent for passive display.
// For user-pinned files, pass toolUseID="".
func NewViewportContent(body string) *ViewportContent {
	vp := viewport.New(0, 0)
	vp.SetContent(body)
	return &ViewportContent{
		body: body,
		vp:   vp,
		th:   theme.Default(),
	}
}

// NewViewportContentWithToolUse constructs a ViewportContent tied to
// a specific agent tool_use block (review card, diff).
func NewViewportContentWithToolUse(body, toolUseID string) *ViewportContent {
	c := NewViewportContent(body)
	c.toolUseID = toolUseID
	return c
}

func (c *ViewportContent) Kind() ContentKind    { return KindViewport }
func (c *ViewportContent) DefaultValue() string { return "" }
func (c *ViewportContent) ToolUseID() string    { return c.toolUseID }

// SetBody replaces the content (used by "r refresh" in the panel).
func (c *ViewportContent) SetBody(body string) {
	c.body = body
	c.vp.SetContent(body)
}

func (c *ViewportContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	var cmd tea.Cmd
	c.vp, cmd = c.vp.Update(msg)
	return c, cmd
}

func (c *ViewportContent) View(width, height int) string {
	// Resize viewport on every View call; cheap and avoids a separate
	// WindowSizeMsg path inside the panel.
	// Deduct 4: Body() adds RoundedBorder (2) + Padding(0,1) (2) horizontally.
	c.vp.Width = width - 4
	if height > 2 {
		c.vp.Height = height - 2
	}
	return c.th.Body().Width(width).Render(c.vp.View())
}
