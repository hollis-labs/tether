package mcpgateway

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

func TestDuplicateWithinOriginHasActionableRemedy(t *testing.T) {
	owner := ToolOwner{Origin: "alpha", Name: "read", Kind: "upstream"}
	err := (&CollisionError{Collisions: []NameCollision{CollidingName("read", owner, owner)}}).Error()
	if !strings.Contains(err, "tools/list response") || strings.Contains(err, "declare tool_prefix on one upstream") {
		t.Fatal(err)
	}
}

func TestProfileStatusHidesExcludedNamingDetails(t *testing.T) {
	ownerA := ToolOwner{Origin: "alpha", Name: "read", Kind: "upstream"}
	ownerB := ToolOwner{Origin: "beta", Name: "read", Kind: "upstream"}
	collision := CollidingName("read", ownerA, ownerB)
	snapshot := Snapshot{Entries: []Entry{{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_read"}}, {Origin: "beta", Tool: &mcpsdk.Tool{Name: "read"}}}, Lint: LintName("beta", "read"), Collisions: []NameCollision{collision}, Origins: []OriginStatus{{ID: "beta", Error: (&CollisionError{Collisions: []NameCollision{collision}}).Error()}}}
	s := Service{Snapshot: func() Snapshot { return snapshot }, Policy: &Policy{Selection: ProfileSelection{Profile: &Profile{Tools: ToolRules{Allow: []string{"alpha_*"}, Deny: []string{"read"}}}}}}
	status := s.Status("")
	if len(status.Lint) != 0 || len(status.Collisions) != 0 || strings.Contains(status.Origins[0].Error, `"read"`) {
		t.Fatalf("hidden names in status %+v", status)
	}
}
