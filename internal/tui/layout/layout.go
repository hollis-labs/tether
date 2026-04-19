// Package layout composes the four-region TUI layout (search input,
// filter chips, flex body, keybinding footer) with Lipgloss.
//
// The scaffold theming lives here too (border and chip styles) until
// Sprint 6 introduces the theme registry. When that lands, these
// styles migrate onto Theme accessors and this package becomes a pure
// composition helper.
package layout

import "github.com/charmbracelet/lipgloss"

// Styles is the collection of Lipgloss styles used to render the
// scaffold layout. Exposed as a struct so T-06's theme pass can swap
// wholesale without chasing bare lipgloss.NewStyle calls across files.
type Styles struct {
	Frame      lipgloss.Style
	Search     lipgloss.Style
	SearchHint lipgloss.Style
	ChipOn     lipgloss.Style
	ChipOff    lipgloss.Style
	Body       lipgloss.Style
	Footer     lipgloss.Style
	FooterKey  lipgloss.Style
	ToastInfo  lipgloss.Style
	ToastError lipgloss.Style
}

// DefaultStyles returns the Sprint-1 scaffold styles. T-06 replaces
// these with theme-driven tokens; Sprints 2-5 should continue to go
// through this helper rather than hand-rolling styles per sub-model.
func DefaultStyles() Styles {
	accent := lipgloss.AdaptiveColor{Light: "#005FAF", Dark: "#7AA2F7"}
	muted := lipgloss.AdaptiveColor{Light: "#6C6C6C", Dark: "#565F89"}
	mutedBg := lipgloss.AdaptiveColor{Light: "#EFEFEF", Dark: "#1F2335"}

	return Styles{
		Frame: lipgloss.NewStyle().Padding(0, 1),
		Search: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(muted).
			Padding(0, 1),
		SearchHint: lipgloss.NewStyle().Foreground(muted).Italic(true),
		ChipOn: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#1A1B26"}).
			Background(accent).
			Padding(0, 1).
			MarginRight(1),
		ChipOff: lipgloss.NewStyle().
			Foreground(muted).
			Background(mutedBg).
			Padding(0, 1).
			MarginRight(1),
		Body: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(muted).
			Padding(0, 1),
		Footer:    lipgloss.NewStyle().Foreground(muted),
		FooterKey: lipgloss.NewStyle().Foreground(accent).Bold(true),
		// Toasts are single-line colored text so they cost only one row
		// each in the vertical budget. Kind distinguishes them by color
		// + leading glyph rather than border or background, which would
		// blow the line budget when several toasts stack.
		ToastInfo: lipgloss.NewStyle().Foreground(accent).Bold(true),
		ToastError: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#A60000", Dark: "#F7768E"}).
			Bold(true),
	}
}

// Render stacks the four regions top-to-bottom. width/height are the
// total terminal dimensions; the caller is responsible for sizing the
// inner content of each region to fit. Render just glues them together
// and pads the overall output to exactly height rows so alt-screen
// repaints don't leave trailing garbage.
func Render(search, chips, body, footer string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	joined := lipgloss.JoinVertical(lipgloss.Left, search, chips, body, footer)
	// Clip to exact height to defend against a sub-region over-reporting
	// its line count; Bubble Tea paints row-by-row so an over-tall frame
	// would push the footer off-screen.
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxHeight(height).
		Render(joined)
}
