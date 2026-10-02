package mcpgateway

import (
	"strings"
	"testing"
)

func TestNameLintReportsWithoutRewriting(t *testing.T) {
	for _, tc := range []struct{ name, code string }{{"alpha.read", "charset"}, {"alpha_" + strings.Repeat("x", 123), "length"}, {"alpha_" + strings.Repeat("x", 45), "client_qualified_length"}, {"read", "origin_prefix"}} {
		findings := LintName("alpha", tc.name)
		found := false
		for _, f := range findings {
			if f.Code == tc.code && f.Name == tc.name {
				found = true
			}
		}
		if !found {
			t.Fatalf("name=%q missing %s: %+v", tc.name, tc.code, findings)
		}
	}
	if got := LintName("alpha", "alpha_read"); len(got) != 0 {
		t.Fatal(got)
	}
}
func TestCollisionDescriptionIsIndependentOfArrivalOrder(t *testing.T) {
	a, b := ToolOwner{Origin: "alpha", Name: "read", Kind: "upstream"}, ToolOwner{Origin: "beta", Name: "read", Kind: "upstream"}
	first := &CollisionError{Collisions: []NameCollision{CollidingName("read", a, b)}}
	second := &CollisionError{Collisions: []NameCollision{CollidingName("read", b, a)}}
	if first.Error() != second.Error() || !strings.Contains(first.Error(), "declare tool_prefix on one upstream") {
		t.Fatalf("%v != %v", first, second)
	}
}
