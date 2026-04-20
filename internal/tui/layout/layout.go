// Package layout composes the four-region TUI layout (search input,
// filter chips, flex body, keybinding footer) with Lipgloss.
//
// After T-06 this package is pure composition: all styling tokens live
// in internal/tui/theme. Render just glues the four already-styled
// region strings together and clamps the output to the terminal height
// so alt-screen repaints don't leave trailing garbage.
package layout

import "github.com/charmbracelet/lipgloss"

// Render stacks the four regions top-to-bottom (search / chips / body
// / footer). width/height are the total terminal dimensions; the
// caller is responsible for sizing the inner content of each region
// to fit. Render just glues them together and pads the overall output
// to exactly height rows so alt-screen repaints don't leave trailing
// garbage.
func Render(search, chips, body, footer string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	joined := lipgloss.JoinVertical(lipgloss.Left, search, chips, body, footer)
	return clampFrame(joined, width, height)
}

// RenderDetail composes a three-region frame (header / body / footer)
// for detail screens that don't carry the main-screen's search+chip
// rows. Same height-clamp discipline as Render.
func RenderDetail(header, body, footer string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	joined := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	return clampFrame(joined, width, height)
}

func clampFrame(content string, width, height int) string {
	// Clip to exact height to defend against a sub-region over-reporting
	// its line count; Bubble Tea paints row-by-row so an over-tall frame
	// would push the footer off-screen.
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxHeight(height).
		Render(content)
}
