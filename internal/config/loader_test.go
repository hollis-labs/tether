package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadExampleCatalog(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
	cat, err := Load(catalogRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cat.Projects["demo"]; !ok {
		t.Fatalf("missing demo project")
	}
	if _, ok := cat.Agents["demo-agent"]; !ok {
		t.Fatalf("missing demo-agent")
	}
	if _, ok := cat.Providers["claude-code"]; !ok {
		t.Fatalf("missing claude-code provider")
	}
	if _, ok := cat.Launches["demo-launch"]; !ok {
		t.Fatalf("missing demo-launch")
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
