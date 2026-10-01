//go:build linux

package mcpadapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// An agent file whose own name is a symlink is refused for a launched agent with
// the typed agent_file_is_symlink code and a message that says what to do, on
// create and on edit, and is never written through. It is not an internal_error,
// and not a conflict.
func TestAgentOps_SymlinkedAgentFileIsATypedRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogRoot, repo, repo2, elsewhere := filepath.Join(base, "catalog"), filepath.Join(base, "repo"), filepath.Join(base, "repo2"), filepath.Join(base, "elsewhere")
	for _, d := range []string{catalogRoot, filepath.Join(repo, ".tether", "agents"), filepath.Join(repo2, ".tether", "agents"), elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	const original = "id: linked\nname: Linked\n"
	target := filepath.Join(elsewhere, "linked.yaml")
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, ".tether", "agents", "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	// A dangling link breaks discovery of its directory, so it lives in a second
	// project, which only creates are aimed at.
	if err := os.Symlink(filepath.Join(elsewhere, "new.yaml"), filepath.Join(repo2, ".tether", "agents", "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo) // project-layer discovery is anchored at the working directory

	a := newAgentOpsAdapter(t, catalogRoot, map[string]config.Project{"p": {RepoRoot: repo}, "q": {RepoRoot: repo2}}, ScopeCatalogWrite)
	a.SetProtectedPaths([]string{catalogRoot})

	for name, call := range map[string]struct {
		tool string
		args map[string]any
	}{
		"edit":                        {"mux_agent_edit", map[string]any{"id": "linked", "system_prompt": "pwned"}},
		"create over a link":          {"mux_agent_create", map[string]any{"id": "linked", "scope": "project", "project": "p", "name": "X"}},
		"create over a dangling link": {"mux_agent_create", map[string]any{"id": "dangling", "scope": "project", "project": "q", "name": "X"}},
	} {
		t.Run(name, func(t *testing.T) {
			res := callAgentTool(t, a, call.tool, call.args)
			text := textOf(res)
			if !res.IsError || !strings.Contains(text, "agent_file_is_symlink") || !strings.Contains(text, "replace the link with a regular file") {
				t.Fatalf("%s = %s; want the typed agent_file_is_symlink refusal", call.tool, text)
			}
			if strings.Contains(text, "internal_error") || strings.Contains(text, "conflict") {
				t.Fatalf("%s = %s; the refusal must not read as an internal error or a conflict", call.tool, text)
			}
		})
	}
	if got, _ := os.ReadFile(target); string(got) != original { //nolint:gosec // test-owned path
		t.Fatalf("the link's target was written through: %q", got)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "new.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a create wrote through a dangling link (stat err = %v)", err)
	}
	// An unprotected adapter (the CLI's rules) is unchanged: it writes through.
	free := newAgentOpsAdapter(t, catalogRoot, map[string]config.Project{"p": {RepoRoot: repo}}, ScopeCatalogWrite)
	if res := callAgentTool(t, free, "mux_agent_edit", map[string]any{"id": "linked", "system_prompt": "edited"}); res.IsError {
		t.Fatalf("an unprotected adapter's edit through a symlink failed: %s", textOf(res))
	}
}
