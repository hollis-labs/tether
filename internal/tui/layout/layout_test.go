package layout

import (
	"strings"
	"testing"
)

func TestRenderContainsAllRegions(t *testing.T) {
	out := Render("SEARCH", "CHIPS", "BODY", "FOOTER", 40, 10)
	for _, token := range []string{"SEARCH", "CHIPS", "BODY", "FOOTER"} {
		if !strings.Contains(out, token) {
			t.Fatalf("expected Render output to contain %q, got:\n%s", token, out)
		}
	}
}

func TestRenderReturnsEmptyOnZeroSize(t *testing.T) {
	if got := Render("a", "b", "c", "d", 0, 10); got != "" {
		t.Fatalf("expected empty output for width=0, got %q", got)
	}
	if got := Render("a", "b", "c", "d", 10, 0); got != "" {
		t.Fatalf("expected empty output for height=0, got %q", got)
	}
}

func TestDefaultStylesDefined(t *testing.T) {
	s := DefaultStyles()
	// Test-runs often strip ANSI colors (no TTY), so comparing rendered
	// output is unreliable. Structural assertions instead: every style
	// should at minimum be non-zero-valued (which the zero Style is).
	// We spot-check a style we know sets padding — zero Style would not.
	if s.Search.GetPaddingLeft() == 0 && s.Search.GetPaddingRight() == 0 {
		t.Fatal("Search style should set horizontal padding")
	}
	if s.ChipOn.GetPaddingLeft() == 0 && s.ChipOn.GetPaddingRight() == 0 {
		t.Fatal("ChipOn style should set horizontal padding")
	}
	if s.Body.GetPaddingLeft() == 0 && s.Body.GetPaddingRight() == 0 {
		t.Fatal("Body style should set horizontal padding")
	}
}
