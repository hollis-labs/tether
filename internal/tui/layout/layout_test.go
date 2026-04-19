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
