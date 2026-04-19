package theme

import "testing"

// Non-trivial render comparisons are unreliable in headless test runs
// (ANSI often gets stripped). These tests assert structural properties:
// that each style carries the padding / bold / border attribute it's
// meant to carry.

func TestDefaultRegionStylesHavePadding(t *testing.T) {
	th := Default()
	cases := []struct {
		name  string
		style func() (int, int)
	}{
		{"Search", func() (int, int) { return th.Search().GetPaddingLeft(), th.Search().GetPaddingRight() }},
		{"ChipOn", func() (int, int) { return th.ChipOn().GetPaddingLeft(), th.ChipOn().GetPaddingRight() }},
		{"ChipOff", func() (int, int) { return th.ChipOff().GetPaddingLeft(), th.ChipOff().GetPaddingRight() }},
		{"Body", func() (int, int) { return th.Body().GetPaddingLeft(), th.Body().GetPaddingRight() }},
		{"Frame", func() (int, int) { return th.Frame().GetPaddingLeft(), th.Frame().GetPaddingRight() }},
	}
	for _, tc := range cases {
		l, r := tc.style()
		if l == 0 && r == 0 {
			t.Errorf("%s: expected horizontal padding, got 0/0", tc.name)
		}
	}
}

func TestSelectedRowIsBold(t *testing.T) {
	th := Default()
	if !th.ResultSelected().GetBold() {
		t.Fatal("ResultSelected should be bold")
	}
}

func TestFooterKeyIsBold(t *testing.T) {
	th := Default()
	if !th.FooterKey().GetBold() {
		t.Fatal("FooterKey should be bold")
	}
}

func TestToastStylesAreBold(t *testing.T) {
	th := Default()
	if !th.ToastInfo().GetBold() || !th.ToastError().GetBold() {
		t.Fatal("Toast styles should be bold for scan-ability")
	}
}

func TestColorAccessorsNonNil(t *testing.T) {
	th := Default()
	// Spot-check that accessors return non-nil colors. AdaptiveColor
	// values are structs, so we just verify the accessor doesn't panic
	// and returns something Lipgloss can work with.
	_ = th.Accent()
	_ = th.Muted()
	_ = th.Border()
	_ = th.Danger()
}
