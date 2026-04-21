package panel

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// MultiChoiceContent renders a list of options with arrow-key navigation.
// No bubbles/list dependency — a custom renderer keeps the code small
// and gives full control over selection rendering.
type MultiChoiceContent struct {
	title      string
	toolUseID  string
	options    []string
	cursor     int
	defaultIdx int
	th         theme.Theme
	keyUp      key.Binding
	keyDown    key.Binding
}

// NewMultiChoiceContent constructs a MultiChoiceContent.
// defaultIdx is the 0-based index of the pre-selected option.
func NewMultiChoiceContent(title string, options []string, defaultIdx int, toolUseID string) *MultiChoiceContent {
	if defaultIdx < 0 || defaultIdx >= len(options) {
		defaultIdx = 0
	}
	return &MultiChoiceContent{
		title:      title,
		toolUseID:  toolUseID,
		options:    options,
		cursor:     defaultIdx,
		defaultIdx: defaultIdx,
		th:         theme.Default(),
		keyUp:      key.NewBinding(key.WithKeys("up", "k")),
		keyDown:    key.NewBinding(key.WithKeys("down", "j")),
	}
}

func (c *MultiChoiceContent) Kind() ContentKind { return KindMultiChoice }
func (c *MultiChoiceContent) ToolUseID() string  { return c.toolUseID }
func (c *MultiChoiceContent) DefaultValue() string {
	if c.defaultIdx >= 0 && c.defaultIdx < len(c.options) {
		return c.options[c.defaultIdx]
	}
	return ""
}

// SelectedValue returns the currently highlighted option string.
func (c *MultiChoiceContent) SelectedValue() string {
	if c.cursor >= 0 && c.cursor < len(c.options) {
		return c.options[c.cursor]
	}
	return ""
}

func (c *MultiChoiceContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, c.keyUp):
			if c.cursor > 0 {
				c.cursor--
			}
		case key.Matches(msg, c.keyDown):
			if c.cursor < len(c.options)-1 {
				c.cursor++
			}
		}
	}
	return c, nil
}

func (c *MultiChoiceContent) View(width, height int) string {
	var sb strings.Builder
	sb.WriteString(c.th.FieldValue().Render(c.title))
	sb.WriteString("\n\n")
	for i, opt := range c.options {
		if i == c.cursor {
			row := c.th.ResultSelected().Width(width - 2).Render("● " + opt)
			sb.WriteString(row)
		} else {
			row := c.th.Footer().Width(width - 2).Render("○ " + opt)
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	hint := c.th.Footer().Render("↑↓ navigate · Enter confirm")
	sb.WriteString(lipgloss.NewStyle().Width(width).Render(hint))
	return sb.String()
}
