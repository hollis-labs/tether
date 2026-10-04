package teamruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRegistryAndBindingAccessBelongsToLegacyAdapter(t *testing.T) {
	// Inspect production source roles, not a fixed list or count of files. A new
	// runtime port cannot silently start depending on legacy registry persistence.
	entries, err := os.ReadDir(".")
	check(t, err)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "legacy_enrollment.go" || name == "legacy_sessions.go" {
			continue
		}
		raw, err := os.ReadFile(name)
		check(t, err)
		for _, finding := range legacyAccess(name, string(raw)) {
			t.Error(finding)
		}

	}
}

func legacyAccess(name, source string) []string {
	if name == "legacy_enrollment.go" || name == "legacy_sessions.go" {
		return nil
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		return []string{err.Error()}
	}
	var findings []string
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path == "github.com/hollis-labs/tether/internal/registry" {
			findings = append(findings, name+": registry import")
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, _ := strconv.Unquote(literal.Value)
		for _, table := range []string{"runtime_bindings", "registry_"} {
			if strings.Contains(value, table) {
				findings = append(findings, name+": "+table)
			}
		}
		return true
	})
	return findings
}
func TestOwnershipGuardExemptsOnlyExactAdapterFilesAndEveryRegistryTable(t *testing.T) {
	source := "package teamruntime\nimport _ \"github.com/hollis-labs/tether/internal/registry\"\nvar q=\"SELECT * FROM registry_shadow\""
	for _, name := range []string{"legacy_extra.go", "sessions.go"} {
		if len(legacyAccess(name, source)) != 2 {
			t.Fatal("ownership escape accepted", name)
		}
	}
	for _, name := range []string{"legacy_enrollment.go", "legacy_sessions.go"} {
		if len(legacyAccess(name, source)) != 0 {
			t.Fatal("adapter refused", name)
		}
	}
}

func TestOwnershipGuardRecognizesRuntimeBindingsLiteral(t *testing.T) {
	findings := legacyAccess("sessions.go", "package teamruntime\nvar q=\"SELECT * FROM runtime_bindings\"")
	if len(findings) != 1 {
		t.Fatal("runtime binding persistence escaped guard", findings)
	}
}
