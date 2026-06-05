package bootexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestPrepareClaudeTUIPlantsBootDirAndNeverResumes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot := t.TempDir()
	bootRoot := t.TempDir()

	plan := &launch.Plan{
		ProviderBrand:  "claude",
		LogicalAgentID: "agent-one",
		RepoRoot:       repoRoot,
		Command:        "claude",
		EnvMode:        "merge",
		Env:            map[string]string{"MUX_MCP_SERVERS": "vanta,clockwork"},
		BootPrompt:     "dynamic boot prompt",
		BootDirOverlay: map[string]string{
			"extra.md": "overlay body\n",
		},
		NativeFiles: []launch.NativeFile{
			{
				Kind:    "skill",
				ID:      "refactor",
				Content: "refactor body\n",
				Mode:    0o600,
			},
		},
	}
	prepared, err := PrepareClaudeTUI(plan, Options{
		BootDirRoot: bootRoot,
		MuxCommand:  "/usr/local/bin/mux",
		MuxArgs:     []string{"--catalog", "/catalog", "mcp", "--proxy"},
		MuxEnv:      []string{"MUX_MCP_SERVERS=vanta,clockwork"},
		ParentEnv:   []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatalf("PrepareClaudeTUI: %v", err)
	}
	defer func() { _ = os.RemoveAll(prepared.BootDir) }()

	if prepared.Command != "claude" {
		t.Fatalf("Command = %q, want claude", prepared.Command)
	}
	if prepared.Dir != prepared.BootDir {
		t.Fatalf("Dir = %q, want boot dir %q", prepared.Dir, prepared.BootDir)
	}
	if !hasArgPair(prepared.Args, "--add-dir", repoRoot) {
		t.Fatalf("Args missing --add-dir %q: %v", repoRoot, prepared.Args)
	}
	if containsArg(prepared.Args, "--resume") {
		t.Fatalf("Args must not include --resume: %v", prepared.Args)
	}

	bootMD := readFile(t, filepath.Join(prepared.BootDir, "boot.md"))
	if bootMD != "dynamic boot prompt" {
		t.Fatalf("boot.md = %q", bootMD)
	}
	claudeMD := readFile(t, filepath.Join(prepared.BootDir, "CLAUDE.md"))
	if !strings.Contains(claudeMD, "dynamic boot prompt") {
		t.Fatalf("CLAUDE.md missing boot prompt: %q", claudeMD)
	}
	mcpJSON := readFile(t, filepath.Join(prepared.BootDir, ".mcp.json"))
	for _, want := range []string{"/usr/local/bin/mux", "MUX_MCP_SERVERS", "vanta,clockwork"} {
		if !strings.Contains(mcpJSON, want) {
			t.Fatalf(".mcp.json missing %q: %s", want, mcpJSON)
		}
	}
	skill := readFile(t, filepath.Join(prepared.BootDir, ".claude", "skills", "refactor.md"))
	if skill != "refactor body\n" {
		t.Fatalf("native skill = %q", skill)
	}
	overlay := readFile(t, filepath.Join(prepared.BootDir, "extra.md"))
	if overlay != "overlay body\n" {
		t.Fatalf("boot overlay = %q", overlay)
	}
}

func TestPrepareClaudeTUIRejectsNonClaude(t *testing.T) {
	_, err := PrepareClaudeTUI(&launch.Plan{ProviderBrand: "codex"}, Options{ParentEnv: []string{}})
	if err == nil {
		t.Fatal("PrepareClaudeTUI returned nil error for non-claude provider")
	}
}

func hasArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(args []string, key string) bool {
	for _, arg := range args {
		if arg == key {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
