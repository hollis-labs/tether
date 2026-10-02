package mcpgateway

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// NameFinding reports an authored wire-name issue without rewriting the name.
type NameFinding struct {
	Origin  string `json:"origin"`
	Name    string `json:"name"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ToolOwner struct {
	Origin string `json:"origin"`
	Name   string `json:"upstream_name"`
	Kind   string `json:"kind"`
}
type NameCollision struct {
	Name   string       `json:"name"`
	Owners [2]ToolOwner `json:"owners"`
}
type CollisionError struct{ Collisions []NameCollision }

func (e *CollisionError) Error() string {
	parts := make([]string, 0, len(e.Collisions))
	for _, c := range e.Collisions {
		parts = append(parts, fmt.Sprintf("final tool name %q collides: %s origin %q tool %q and %s origin %q tool %q; declare tool_prefix on one upstream", c.Name, c.Owners[0].Kind, c.Owners[0].Origin, c.Owners[0].Name, c.Owners[1].Kind, c.Owners[1].Origin, c.Owners[1].Name))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
func CollidingName(name string, a, b ToolOwner) NameCollision {
	key := func(o ToolOwner) string { return o.Origin + "\x00" + o.Kind + "\x00" + o.Name }
	if key(b) < key(a) {
		a, b = b, a
	}
	return NameCollision{Name: name, Owners: [2]ToolOwner{a, b}}
}

var portableName = regexp.MustCompile(`^[a-z0-9_]+$`)

func LintName(origin, name string) []NameFinding {
	var out []NameFinding
	add := func(code, message string) {
		out = append(out, NameFinding{Origin: origin, Name: name, Code: code, Message: message})
	}
	if !portableName.MatchString(name) {
		add("charset", "final name should contain only [a-z0-9_]; declare tool_prefix or fix the upstream name, names are never normalized")
	}
	if utf8.RuneCountInString(name) > 128 {
		add("length", "final name exceeds 128 characters")
	}
	if len("mcp__tether__"+name) > 63 {
		add("client_qualified_length", "mcp__tether__ plus the final name exceeds the conservative 63-byte client limit")
	}
	if !strings.HasPrefix(name, origin+"_") {
		add("origin_prefix", fmt.Sprintf("final name should identify origin %q with %q; declare tool_prefix if needed", origin, origin+"_"))
	}
	return out
}

// ValidateOriginIDs is scoped to gateway startup/doctor, never generic catalog loading.
func ValidateOriginIDs(ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "tether" {
			return fmt.Errorf("MCP upstream origin %q is reserved for native Tether tools; rename the catalog upstream ID", id)
		}
		if id == "" {
			return fmt.Errorf("MCP upstream origin ID is empty; declare a nonempty catalog ID")
		}
		if seen[id] {
			return fmt.Errorf("duplicate MCP upstream origin ID %q; declare distinct catalog IDs", id)
		}
		seen[id] = true
	}
	return nil
}
