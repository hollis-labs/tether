package launch

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/config"
)

func TestResolveDemoLaunch(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
	cat, err := config.Load(catalogRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	plan, err := Resolve(cat, Input{LaunchID: "demo-launch", CatalogRoot: catalogRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Command != "claude" {
		t.Fatalf("want command=claude, got %q", plan.Command)
	}
	if !strings.Contains(plan.BootPrompt, "Agent Mux") {
		t.Fatalf("boot prompt missing common fragment: %q", plan.BootPrompt)
	}
}
