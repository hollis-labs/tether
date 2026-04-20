// Package theme owns every color, border, and padding token the TUI
// renders with. Exposing one Theme value (and a matching constructor
// per visual variant) is the single seam Sprint 6's alternate-theme
// pass will swap through: each sub-model asks the theme for a named
// style rather than building lipgloss styles inline.
//
// T-06 ships exactly one theme — Default(), a dark-first theme with
// Lipgloss AdaptiveColor so light-terminal users still get a readable
// render. Sprint 6 adds a HighContrast() variant and a palette command
// to pick between them at runtime.
package theme

import "github.com/charmbracelet/lipgloss"

// Theme is the token + style registry. Fields are unexported so
// callers always go through the named accessors; that keeps the
// Sprint-6 swap a single-line change.
type Theme struct {
	// Color tokens — exposed via Accent()/Muted()/Border() for any
	// caller that needs raw colors outside the pre-built styles.
	accent      lipgloss.TerminalColor
	accentSoft  lipgloss.TerminalColor
	muted       lipgloss.TerminalColor
	mutedBg     lipgloss.TerminalColor
	border      lipgloss.TerminalColor
	text        lipgloss.TerminalColor
	textInverse lipgloss.TerminalColor
	danger      lipgloss.TerminalColor
}

// Default returns the dark-first scaffold theme. Uses AdaptiveColor
// throughout so light-background terminals still render readable —
// even though the palette was picked with dark in mind. The named
// hex values are borrowed from the Tokyo Night palette on the dark
// side and a Solarized-inspired neutral set on the light side.
func Default() Theme {
	return Theme{
		accent:      lipgloss.AdaptiveColor{Light: "#005FAF", Dark: "#7AA2F7"},
		accentSoft:  lipgloss.AdaptiveColor{Light: "#D6E4FF", Dark: "#2A3454"},
		muted:       lipgloss.AdaptiveColor{Light: "#6C6C6C", Dark: "#565F89"},
		mutedBg:     lipgloss.AdaptiveColor{Light: "#EFEFEF", Dark: "#1F2335"},
		border:      lipgloss.AdaptiveColor{Light: "#B0B0B0", Dark: "#3B4261"},
		text:        lipgloss.AdaptiveColor{Light: "#1A1B26", Dark: "#C0CAF5"},
		textInverse: lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#1A1B26"},
		danger:      lipgloss.AdaptiveColor{Light: "#A60000", Dark: "#F7768E"},
	}
}

// ---- color accessors --------------------------------------------------

func (t Theme) Accent() lipgloss.TerminalColor { return t.accent }
func (t Theme) Muted() lipgloss.TerminalColor  { return t.muted }
func (t Theme) Border() lipgloss.TerminalColor { return t.border }
func (t Theme) Danger() lipgloss.TerminalColor { return t.danger }

// Header is the title bar atop detail screens. Bold accent-colored
// text on a muted background, padded.
func (t Theme) Header() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(t.accent).
		Bold(true).
		Padding(0, 1)
}

// FieldLabel is the left-side label for a detail-screen field row.
func (t Theme) FieldLabel() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.muted).Bold(true)
}

// FieldValue is the right-side value for a detail-screen field row.
func (t Theme) FieldValue() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.text)
}

// ---- region styles ----------------------------------------------------

// Frame is the outermost padding applied to multi-item rows
// (chip row, footer block) so their contents don't hug the terminal
// edge.
func (t Theme) Frame() lipgloss.Style {
	return lipgloss.NewStyle().Padding(0, 1)
}

// Search frames the top search input with a rounded, muted border so
// the input feels like an integrated component rather than a floating
// line.
func (t Theme) Search() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.border).
		Padding(0, 1)
}

// SearchHint styles the textinput's placeholder text.
func (t Theme) SearchHint() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.muted).Italic(true)
}

// ChipOn / ChipOff style the filter chips. On-state uses the accent
// as a background fill so the enabled filter reads as "hot".
func (t Theme) ChipOn() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(t.textInverse).
		Background(t.accent).
		Padding(0, 1).
		MarginRight(1)
}

func (t Theme) ChipOff() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(t.muted).
		Background(t.mutedBg).
		Padding(0, 1).
		MarginRight(1)
}

// Body wraps the results viewport with a rounded border so the result
// set reads as a distinct pane.
func (t Theme) Body() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.border).
		Padding(0, 1)
}

// ResultUnselected is the passthrough style for ordinary rows — no
// background, default foreground. Kept explicit so future themes can
// introduce a subtle zebra-stripe or similar without chasing callers.
func (t Theme) ResultUnselected() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.text)
}

// ResultSelected renders the highlighted row with a soft accent fill
// plus bold text. The soft-accent rather than full-accent keeps the
// subtitle readable at the current palette without a separate subtitle
// override.
func (t Theme) ResultSelected() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(t.text).
		Background(t.accentSoft).
		Bold(true)
}

// Footer / FooterKey are the muted-key and accent-for-key-names split
// used by renderFooter.
func (t Theme) Footer() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.muted)
}

func (t Theme) FooterKey() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.accent).Bold(true)
}

// ToastInfo / ToastError: single-line styled notifications. Kind
// distinguishes by color + glyph (the glyph is owned by the caller).
func (t Theme) ToastInfo() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.accent).Bold(true)
}

func (t Theme) ToastError() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.danger).Bold(true)
}
