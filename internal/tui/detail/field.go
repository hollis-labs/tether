// Package detail hosts the typed detail screens for each catalog /
// runtime object. Every screen renders the same header / body /
// footer shape (via layout.RenderDetail) with a type-specific set of
// labeled fields in the body.
//
// Entry convention: callers construct a screen with New<Type>Screen
// (e.g., NewProjectScreen) and push it onto the root's screen.Stack
// via screen.Push. Esc / Left-arrow pops back.
package detail

import (
	"fmt"
	"strings"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// field is an internal label+value pair. Lists render with one bullet
// per entry; maps render as indented key: value lines.
type field struct {
	label string
	value string
	list  []string // non-nil → render as indented bullets
	pairs map[string]string
}

// renderFields produces the detail-screen body content: one labeled
// row per field, aligned and colored via the theme. Lists and maps
// render across multiple lines with the label on the first line.
func renderFields(th theme.Theme, fields []field) string {
	// Compute label width for alignment. Short max so values get room.
	maxLabel := 0
	for _, f := range fields {
		if l := len(f.label); l > maxLabel {
			maxLabel = l
		}
	}
	if maxLabel > 20 {
		maxLabel = 20
	}

	var sb strings.Builder
	for _, f := range fields {
		label := th.FieldLabel().Render(padRight(f.label, maxLabel))
		switch {
		case len(f.list) > 0:
			sb.WriteString(label + "  " + th.FieldValue().Render(bulletFirst(f.list[0])))
			sb.WriteByte('\n')
			for _, item := range f.list[1:] {
				sb.WriteString(strings.Repeat(" ", maxLabel+2))
				sb.WriteString(th.FieldValue().Render(bulletFirst(item)))
				sb.WriteByte('\n')
			}
		case len(f.pairs) > 0:
			sb.WriteString(label + "\n")
			for k, v := range f.pairs {
				sb.WriteString(strings.Repeat(" ", maxLabel+2))
				sb.WriteString(th.FieldValue().Render(fmt.Sprintf("%s: %s", k, v)))
				sb.WriteByte('\n')
			}
		default:
			sb.WriteString(label + "  " + th.FieldValue().Render(nonEmpty(f.value)))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func bulletFirst(s string) string { return "· " + s }

func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}
