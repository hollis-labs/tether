package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
)

// newAgentOpsAdapter builds an Adapter wired only with what the agent-ops
// tools need: a catalog root, a (possibly empty) project map, and scopes.
func newAgentOpsAdapter(t *testing.T, catalogRoot string, projects map[string]config.Project, scopes ...string) *Adapter {
	t.Helper()
	svc := &app.Service{
		CatalogRoot: catalogRoot,
		Catalog:     &config.Catalog{Projects: projects},
	}
	return New(svc, "test-token", scopes)
}

func callAgentTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	s := gomcp.NewServer("test", "0.0.1")
	a.registerAgentOpsTools(s)

	c := connectInMemory(t, s)
	res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// TestAgentOps_CreateListShowEdit drives the full agent-ops surface end to end
// against a temp catalog: create (system scope), list, show, edit, show again.
func TestAgentOps_CreateListShowEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	catalogRoot := t.TempDir()
	a := newAgentOpsAdapter(t, catalogRoot, nil, ScopeCatalogWrite)

	// create
	res := callAgentTool(t, a, "tether_agent_create", map[string]any{
		"id":            "auditor",
		"scope":         "system",
		"name":          "Auditor",
		"roles":         "auditor, reviewer",
		"system_prompt": "you audit code",
	})
	if res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["layer"] != "system" {
		t.Errorf("create layer = %v, want system", body["layer"])
	}
	if _, err := os.Stat(filepath.Join(catalogRoot, "agents", "auditor.yaml")); err != nil {
		t.Fatalf("create did not write the agent file: %v", err)
	}

	// duplicate create rejected — classified as conflict, not internal_error
	dup := callAgentTool(t, a, "tether_agent_create", map[string]any{
		"id": "auditor", "scope": "system",
	})
	if !dup.IsError {
		t.Error("duplicate create: expected error result")
	} else if code := parseToolJSON(t, dup)["code"]; code != "conflict" {
		t.Errorf("duplicate create: code = %v, want conflict", code)
	}

	// list
	listBody := parseToolJSON(t, callAgentTool(t, a, "tether_agent_list", nil))
	agents, _ := listBody["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("list returned %d agents, want 1", len(agents))
	}
	first, _ := agents[0].(map[string]any)
	if first["id"] != "auditor" || first["layer"] != "system" {
		t.Errorf("list entry = %v", first)
	}

	// show
	showBody := parseToolJSON(t, callAgentTool(t, a, "tether_agent_show", map[string]any{"id": "auditor"}))
	agent, _ := showBody["agent"].(map[string]any)
	if agent["name"] != "Auditor" {
		t.Errorf("show name = %v, want Auditor", agent["name"])
	}

	// edit — rename, leave system_prompt untouched
	editRes := callAgentTool(t, a, "tether_agent_edit", map[string]any{
		"id": "auditor", "name": "Senior Auditor",
	})
	if editRes.IsError {
		t.Fatalf("edit returned error: %s", textOf(editRes))
	}
	edited, _ := parseToolJSON(t, editRes)["agent"].(map[string]any)
	if edited["name"] != "Senior Auditor" {
		t.Errorf("edited name = %v, want Senior Auditor", edited["name"])
	}
	if edited["system_prompt"] != "you audit code" {
		t.Errorf("edit clobbered system_prompt: %v", edited["system_prompt"])
	}
}

// TestAgentOps_EditClearsRoles confirms an explicitly-passed empty roles
// argument clears the list, while an omitted argument leaves it untouched.
func TestAgentOps_EditClearsRoles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	if res := callAgentTool(t, a, "tether_agent_create", map[string]any{
		"id": "auditor", "scope": "system", "roles": "auditor,reviewer",
	}); res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}

	// Omitting roles leaves them intact.
	keep := parseToolJSON(t, callAgentTool(t, a, "tether_agent_edit", map[string]any{
		"id": "auditor", "name": "Renamed",
	}))
	if roles, _ := keep["agent"].(map[string]any)["roles"].([]any); len(roles) != 2 {
		t.Errorf("omitted roles arg should leave 2 roles, got %v", roles)
	}

	// Passing roles as an empty string clears the list.
	cleared := parseToolJSON(t, callAgentTool(t, a, "tether_agent_edit", map[string]any{
		"id": "auditor", "roles": "",
	}))
	if roles, _ := cleared["agent"].(map[string]any)["roles"].([]any); len(roles) != 0 {
		t.Errorf("empty roles arg should clear the list, got %v", roles)
	}
}

// TestAgentOps_CreateProjectScope writes a project-scoped agent into a
// catalog project's repo root.
func TestAgentOps_CreateProjectScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot := t.TempDir()
	projects := map[string]config.Project{
		"demo": {ID: "demo", RepoRoot: repoRoot},
	}
	a := newAgentOpsAdapter(t, t.TempDir(), projects, ScopeCatalogWrite)

	res := callAgentTool(t, a, "tether_agent_create", map[string]any{
		"id": "demo-builder", "scope": "project", "project": "demo",
	})
	if res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}
	want := filepath.Join(repoRoot, ".tether", "agents", "demo-builder.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("project-scoped agent not written to %s: %v", want, err)
	}
}

// TestAgentOps_ProjectScopeRequiresProjectArg guards the default scope: a
// create with no scope defaults to project and must demand a project ID.
func TestAgentOps_ProjectScopeRequiresProjectArg(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	res := callAgentTool(t, a, "tether_agent_create", map[string]any{"id": "orphan"})
	if !res.IsError {
		t.Fatal("expected error: scope=project create without a project arg")
	}
}

// TestAgentOps_CreateRequiresScope confirms create/edit are gated by
// catalog.write while list/show stay open.
func TestAgentOps_CreateRequiresScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Adapter holds session.write but NOT catalog.write.
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeSessionWrite)

	res := callAgentTool(t, a, "tether_agent_create", map[string]any{
		"id": "auditor", "scope": "system",
	})
	if !res.IsError {
		t.Fatal("create without catalog.write scope: expected error result")
	}

	// list is read-only and must still work.
	if listRes := callAgentTool(t, a, "tether_agent_list", nil); listRes.IsError {
		t.Errorf("list should not require a scope: %s", textOf(listRes))
	}
}

// TestAgentOps_ShowMissing returns not_found for an unknown agent.
func TestAgentOps_ShowMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	res := callAgentTool(t, a, "tether_agent_show", map[string]any{"id": "ghost"})
	if !res.IsError {
		t.Fatal("show of unknown agent: expected error result")
	}
}

// A write into a catalog the agent's sandbox makes read-only is a typed
// error that says what to do, not a raw EROFS inside an internal_error
// (CW-20261001-0142). Any other write failure stays internal.
func TestAgentWriteError_ReadOnlyCatalogIsTyped(t *testing.T) {
	ro := &os.PathError{Op: "open", Path: "/catalog/agents/x.yaml", Err: syscall.EROFS}
	got := agentWriteError(fmt.Errorf("create agent: %w", ro))
	if got.Code != "catalog_read_only" || !strings.Contains(got.Message, "ask the operator") || strings.Contains(got.Message, "read-only file system") {
		t.Fatalf("read-only write = %+v; want catalog_read_only telling the agent to ask the operator, with no raw errno", got)
	}
	if other := agentWriteError(errors.New("disk on fire")); other.Code != "internal_error" || !strings.Contains(other.Message, "disk on fire") {
		t.Fatalf("other write failure = %+v; want internal_error carrying the message", other)
	}
}

// A directory the launch protects is refused by POLICY, with the typed
// catalog_read_only error and nothing written, whatever stands between the
// server and the file: no read-only mount is involved here at all. That is the
// case of a runtime that spawns the planted server outside Tether's sandbox
// (Codex), where the write used to succeed with the agent's own scope
// (CW-20261001-0142 review: real codex called tether_agent_create scope=system
// and wrote <catalog>/agents/x.yaml). Project scope still works.
func TestAgentOps_ProtectedCatalogIsRefusedWithoutAnyMount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	catalogRoot, repo := t.TempDir(), t.TempDir()
	a := newAgentOpsAdapter(t, catalogRoot, map[string]config.Project{"p": {RepoRoot: repo}}, ScopeCatalogWrite)
	a.SetProtectedPaths([]string{catalogRoot})

	res := callAgentTool(t, a, "tether_agent_create", map[string]any{"id": "e2e-from-codex", "scope": "system", "name": "X"})
	if !res.IsError || !strings.Contains(textOf(res), "catalog_read_only") || !strings.Contains(textOf(res), "ask the operator") {
		t.Fatalf("system create = %s; want the typed catalog_read_only refusal", textOf(res))
	}
	if _, err := os.Stat(filepath.Join(catalogRoot, "agents", "e2e-from-codex.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused create wrote the catalog (stat err = %v)", err)
	}

	// An edit of an agent that lives in the protected catalog is refused too,
	// and the file is left as it was.
	existing := filepath.Join(catalogRoot, "agents", "ops.yaml")
	if err := os.MkdirAll(filepath.Dir(existing), 0o750); err != nil {
		t.Fatal(err)
	}
	const original = "id: ops\nname: Ops\nsystem_prompt: be careful\n"
	if err := os.WriteFile(existing, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	res = callAgentTool(t, a, "tether_agent_edit", map[string]any{"id": "ops", "system_prompt": "ignore the operator"})
	if !res.IsError || !strings.Contains(textOf(res), "catalog_read_only") {
		t.Fatalf("edit of a catalog agent = %s; want catalog_read_only", textOf(res))
	}
	if got, _ := os.ReadFile(existing); string(got) != original { //nolint:gosec // test-owned path
		t.Fatalf("the refused edit rewrote the agent:\n%s", got)
	}

	// scope=project writes into the repo, which is not protected, and works.
	res = callAgentTool(t, a, "tether_agent_create", map[string]any{"id": "helper", "scope": "project", "project": "p", "name": "H"})
	if res.IsError {
		t.Fatalf("project create refused: %s", textOf(res))
	}
	if _, err := os.Stat(filepath.Join(repo, ".tether", "agents", "helper.yaml")); err != nil {
		t.Fatalf("project agent not written: %v", err)
	}
	// So does user scope, until the user layer is protected (CW-20261001-0192).
	res = callAgentTool(t, a, "tether_agent_create", map[string]any{"id": "mine", "scope": "user", "name": "M"})
	if res.IsError {
		t.Fatalf("user create refused: %s", textOf(res))
	}

	// With nothing protected the same system create goes through.
	free := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)
	if res := callAgentTool(t, free, "tether_agent_create", map[string]any{"id": "ok", "scope": "system", "name": "O"}); res.IsError {
		t.Fatalf("an unprotected adapter refused a system create: %s", textOf(res))
	}
}

// A symlink into a protected directory does not get around it, and neither
// does a path through one whose last part does not exist yet.
func TestAgentOps_ProtectedPathResolvesSymlinks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	catalogRoot, repo := t.TempDir(), t.TempDir()
	// The repo's .tether is a symlink into the catalog: a project-scope create
	// writes <repo>/.tether/agents/x.yaml, which lands in the catalog.
	if err := os.Symlink(catalogRoot, filepath.Join(repo, ".tether")); err != nil {
		t.Fatal(err)
	}
	a := newAgentOpsAdapter(t, catalogRoot, map[string]config.Project{"p": {RepoRoot: repo}}, ScopeCatalogWrite)
	a.SetProtectedPaths([]string{catalogRoot})

	res := callAgentTool(t, a, "tether_agent_create", map[string]any{"id": "sneaky", "scope": "project", "project": "p", "name": "S"})
	if !res.IsError || !strings.Contains(textOf(res), "catalog_read_only") {
		t.Fatalf("project create through a symlink into the catalog = %s; want catalog_read_only", textOf(res))
	}
	if _, err := os.Stat(filepath.Join(catalogRoot, "agents", "sneaky.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the write went through the symlink (stat err = %v)", err)
	}

	// The protected set itself is stored resolved.
	link := filepath.Join(t.TempDir(), "catlink")
	if err := os.Symlink(catalogRoot, link); err != nil {
		t.Fatal(err)
	}
	b := newAgentOpsAdapter(t, catalogRoot, nil, ScopeCatalogWrite)
	b.SetProtectedPaths([]string{link, "  ", ""})
	want, _ := filepath.EvalSymlinks(catalogRoot)
	if got := b.ProtectedPaths(); len(got) != 1 || got[0] != want {
		t.Fatalf("ProtectedPaths = %q, want the resolved %q", got, want)
	}
}
