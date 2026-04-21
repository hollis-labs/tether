package panel

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// TextInputContent wraps bubbles/textinput for single-line agent prompts.
type TextInputContent struct {
	title     string
	toolUseID string
	input     textinput.Model
	th        theme.Theme
}

// NewTextInputContent constructs a TextInputContent with an optional
// prefilled default value.
func NewTextInputContent(title, defaultValue, toolUseID string) *TextInputContent {
	ti := textinput.New()
	ti.Placeholder = "type here…"
	ti.SetValue(defaultValue)
	ti.Focus()
	return &TextInputContent{
		title:     title,
		toolUseID: toolUseID,
		input:     ti,
		th:        theme.Default(),
	}
}

func (c *TextInputContent) Kind() ContentKind    { return KindTextInput }
func (c *TextInputContent) DefaultValue() string { return c.input.Value() }
func (c *TextInputContent) ToolUseID() string    { return c.toolUseID }

// CurrentValue returns whatever the user has typed (may differ from
// the initial default).
func (c *TextInputContent) CurrentValue() string { return c.input.Value() }

func (c *TextInputContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(msg)
	return c, cmd
}

func (c *TextInputContent) View(width, height int) string {
	c.input.Width = width - 4
	title := c.th.FieldValue().Render(c.title)
	input := c.th.Search().Width(width - 2).Render(c.input.View())
	hint := c.th.Footer().Render("Enter to confirm")
	return title + "\n\n" + input + "\n\n" + hint
}
