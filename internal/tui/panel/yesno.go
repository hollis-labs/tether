package panel

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// YesNoContent renders a two-option yes/no prompt with pre-selected default.
type YesNoContent struct {
	title      string
	toolUseID  string
	options    [2]string // always ["yes", "no"]
	cursor     int       // 0=yes, 1=no
	defaultVal string
	th         theme.Theme
	keyUp      key.Binding
	keyDown    key.Binding
	keyYes     key.Binding
	keyNo      key.Binding
}

// NewYesNoContent constructs a YesNoContent. defaultChoice must be "yes" or "no".
func NewYesNoContent(title, defaultChoice, toolUseID string) *YesNoContent {
	cursor := 0
	if defaultChoice == "no" {
		cursor = 1
	}
	return &YesNoContent{
		title:      title,
		toolUseID:  toolUseID,
		options:    [2]string{"yes", "no"},
		cursor:     cursor,
		defaultVal: defaultChoice,
		th:         theme.Default(),
		keyUp:      key.NewBinding(key.WithKeys("up", "k")),
		keyDown:    key.NewBinding(key.WithKeys("down", "j")),
		keyYes:     key.NewBinding(key.WithKeys("y", "Y")),
		keyNo:      key.NewBinding(key.WithKeys("n", "N")),
	}
}

func (c *YesNoContent) Kind() ContentKind    { return KindYesNo }
func (c *YesNoContent) DefaultValue() string { return c.defaultVal }
func (c *YesNoContent) ToolUseID() string    { return c.toolUseID }

func (c *YesNoContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, c.keyUp):
			if c.cursor > 0 {
				c.cursor--
			}
		case key.Matches(msg, c.keyDown):
			if c.cursor < 1 {
				c.cursor++
			}
		case key.Matches(msg, c.keyYes):
			c.cursor = 0
		case key.Matches(msg, c.keyNo):
			c.cursor = 1
		}
	}
	return c, nil
}

// SelectedValue returns the currently highlighted option.
func (c *YesNoContent) SelectedValue() string {
	return c.options[c.cursor]
}

func (c *YesNoContent) View(width, height int) string {
	var sb strings.Builder
	sb.WriteString(c.th.FieldValue().Render(c.title))
	sb.WriteString("\n\n")
	for i, opt := range c.options {
		label := strings.ToUpper(opt[:1]) + opt[1:]
		if i == c.cursor {
			row := c.th.ResultSelected().Width(width - 2).Render("● " + label)
			sb.WriteString(row)
		} else {
			row := c.th.Footer().Width(width - 2).Render("○ " + label)
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	hint := c.th.Footer().Render("↑↓ navigate · Enter confirm · Y/N shortcut")
	sb.WriteString(lipgloss.NewStyle().Width(width).Render(hint))
	return sb.String()
}
