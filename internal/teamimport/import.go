// Package teamimport converts an explicitly reviewed tmux/Cairn baseline into
// supported Tether catalog inputs. It never executes Cairn, a provider, or a
// credential helper, and never reads ambient provider configuration.
package teamimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"gopkg.in/yaml.v3"
)

// Manifest is non-secret input. Lists must be explicit, including empty grants.
// MCP contains reviewed upstream definitions, not credentials or their values.
type Manifest struct {
	Version          int      `json:"version"`
	NonSecretSources bool     `json:"non_secret_sources"`
	TeamHome         string   `json:"team_home"`
	TeamSession      string   `json:"team_session"`
	TetherCommand    string   `json:"tether_command"`
	WorktreesRoot    string   `json:"worktrees_root"`
	Path             string   `json:"path"`
	Roles            []Role   `json:"roles"`
	MCP              []Server `json:"mcp"`
}

// Server contains reviewed transport settings and credential references.
// A token_file names an existing private file; the importer never opens it.
type Server struct {
	ID                    string            `json:"id"`
	Transport             string            `json:"transport"`
	Command               string            `json:"command,omitempty"`
	Args                  []string          `json:"args,omitempty"`
	Env                   map[string]string `json:"env,omitempty"`
	URL                   string            `json:"url,omitempty"`
	Token                 string            `json:"token,omitempty"`
	TokenFile             string            `json:"token_file,omitempty"`
	ProxyServiceTokenFile string            `json:"proxy_service_token_file,omitempty"`
	ToolPrefix            string            `json:"tool_prefix,omitempty"`
	Scopes                []string          `json:"scopes,omitempty"`
	AllowUnconfinedRemote bool              `json:"allow_unconfined_remote,omitempty"`
}

func (s Server) entry() config.MCPServerEntry {
	return config.MCPServerEntry{ID: s.ID, Transport: s.Transport, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Token: s.Token, TokenFile: s.TokenFile, ProxyServiceTokenFile: s.ProxyServiceTokenFile, ToolPrefix: s.ToolPrefix, Scopes: s.Scopes, AllowUnconfinedRemote: s.AllowUnconfinedRemote}
}

// Role records the effective selection, not an inferred host default. An
// existing msg://agent/agent-mux identity is preserved as the catalog agent ID.
type Role struct {
	Name             string   `json:"name"`
	Role             string   `json:"role"`
	Project          string   `json:"project"`
	Scope            string   `json:"scope"`
	BootDir          string   `json:"boot_dir"`
	URN              string   `json:"urn"`
	Runtime          string   `json:"runtime"`
	Model            string   `json:"model"`
	Effort           string   `json:"effort"`
	PermissionMode   string   `json:"permission_mode"`
	Prompt           string   `json:"prompt"`
	BaselineEvidence string   `json:"baseline_evidence"`
	NativeScopes     []string `json:"native_scopes"`
	MCPServers       []string `json:"mcp_servers"`
	MCPTools         []string `json:"mcp_tools"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Write produces a new private standalone catalog. Existing destinations are
// refused; all validation and source reads complete before any output is made.
func Write(m Manifest, output string) error {
	root, err := safeDestination(output, m)
	if err != nil {
		return err
	}
	files, err := build(m, root)
	if err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return fmt.Errorf("create fresh import destination: %w", err)
	}
	for _, name := range []string{"control", "state", "tmp", "workspaces"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			return err
		}
	}
	for _, r := range m.Roles {
		for _, name := range []string{"tmp", "workspaces"} {
			if err := os.Mkdir(filepath.Join(root, name, r.Name), 0o700); err != nil {
				return err
			}
		}
	}
	// A failed import is retained for diagnosis. Never remove a caller's tree.
	for _, name := range sortedKeys(files) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, files[name], 0o600); err != nil {
			return err
		}
	}
	return nil
}

func safeDestination(output string, m Manifest) (string, error) {
	if !filepath.IsAbs(output) {
		return "", errors.New("output must be an absolute fresh directory")
	}
	root := filepath.Clean(output)
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("output already exists or cannot be inspected")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		return "", errors.New("output parent must already exist")
	}
	root = filepath.Join(parent, filepath.Base(root))
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	blocked := []string{filepath.Join(home, ".tether"), filepath.Join(home, ".codex"), filepath.Join(home, ".claude"), filepath.Join(home, ".gemini"), m.TeamHome}
	for _, r := range m.Roles {
		blocked = append(blocked, r.BootDir)
	}
	for _, p := range blocked {
		if p == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil && beneath(resolved, root) {
			return "", errors.New("output must be outside shared team, source boot, catalog and provider-home trees")
		}
	}
	return root, nil
}

func build(m Manifest, root string) (map[string][]byte, error) {
	if m.Version != 1 || !m.NonSecretSources || len(m.Roles) == 0 {
		return nil, errors.New("version 1, non_secret_sources=true and roles are required")
	}
	if m.TeamSession == "" || m.TetherCommand == "" || m.Path == "" {
		return nil, errors.New("explicit team_session, tether_command and path are required")
	}
	for _, path := range []string{m.TeamHome, m.WorktreesRoot} {
		if err := directory(path); err != nil {
			return nil, err
		}
	}
	files := map[string][]byte{}
	put := func(name string, value any) error {
		b, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		files[name] = b
		return nil
	}
	servers := map[string]bool{}
	for _, server := range m.MCP {
		entry := server.entry()
		if !identifier.MatchString(entry.ID) || servers[entry.ID] || !entry.IsEnabled() {
			return nil, errors.New("MCP IDs must be unique, enabled and safe identifiers")
		}
		if err := nonSecretMCP(entry); err != nil {
			return nil, err
		}
		servers[entry.ID] = true
		if err := put(filepath.Join("mcp-servers", entry.ID+".yaml"), entry); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	projects := map[string]string{}
	usedServers := map[string]bool{}
	for _, r := range m.Roles {
		const prefix = "msg://agent/agent-mux/"
		agentID := strings.TrimPrefix(r.URN, prefix)
		if !strings.HasPrefix(r.URN, prefix) || !identifier.MatchString(agentID) || !identifier.MatchString(r.Name) || !identifier.MatchString(r.Project) || !identifier.MatchString(r.Role) || seen[r.Name] || seen[agentID] {
			return nil, errors.New("roles require unique names, original agent-mux URNs and safe identifiers")
		}
		seen[r.Name], seen[agentID] = true, true
		if r.Runtime != "codex" || r.Model == "" || !slices.Contains([]string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, r.Effort) || r.Prompt == "" || r.BaselineEvidence == "" {
			return nil, errors.New("codex role requires explicit model, effort, prompt and baseline_evidence; other runtimes are not converted")
		}
		if r.PermissionMode != "bypass" && r.PermissionMode != "default" {
			return nil, errors.New("explicit permission_mode must be bypass or default")
		}
		if r.MCPServers == nil || r.MCPTools == nil || r.NativeScopes == nil {
			return nil, errors.New("mcp_servers, mcp_tools and native_scopes must be explicit baseline arrays; omitted/null grants are refused")
		}
		if err := (mcpgateway.Profile{Tools: mcpgateway.ToolRules{Allow: r.MCPTools}}).Validate(); err != nil {
			return nil, err
		}
		for _, id := range r.MCPServers {
			if !servers[id] {
				return nil, errors.New("every granted MCP server requires its reviewed non-secret definition")
			}
			usedServers[id] = true
		}
		for _, p := range []string{r.Scope, r.BootDir} {
			if err := directory(p); err != nil {
				return nil, err
			}
		}
		if scope, ok := projects[r.Project]; ok && scope != r.Scope {
			return nil, errors.New("roles in the same project must have the same declared scope")
		}
		projects[r.Project] = r.Scope
		roleFiles, err := readRoleFiles(r.BootDir)
		if err != nil {
			return nil, err
		}
		body := string(roleFiles["AGENTS.md"].data)
		env := map[string]string{"TEAM_NAME": r.Name, "TEAM_ROLE": r.Role, "TEAM_PROJECT": r.Project, "TEAM_RUNTIME": r.Runtime, "TEAM_URN": r.URN, "TEAM_HOME": m.TeamHome, "TEAM_SESSION": m.TeamSession, "TEAM_TETHER": m.TetherCommand, "PATH": m.Path, "TMPDIR": filepath.Join(root, "tmp", r.Name), "GOTMPDIR": filepath.Join(root, "tmp", r.Name)}
		tools, err := json.Marshal(r.MCPTools)
		if err != nil {
			return nil, err
		}
		// The launch itself carries the same ceiling, including when a caller
		// uses --launch without the generated boot profile.
		env[mcpgateway.ToolsEnv] = string(tools)
		env["TETHER_MCP_SERVERS"] = strings.Join(r.MCPServers, ",")
		roots, _ := json.Marshal([]string{r.Scope, m.TeamHome, m.WorktreesRoot})
		args := []string{"-c", "model=" + strconv.Quote(r.Model), "-c", "model_reasoning_effort=" + strconv.Quote(r.Effort), "-c", "shell_environment_policy.set=" + inlineTable(env), "-c", "sandbox_workspace_write.writable_roots=" + string(roots)}
		providerID := "team-" + r.Name
		provider := config.Provider{ID: providerID, Type: "cli-goprovider", Provider: "codex", Adapter: "codex", RuntimeKind: "jsonrpc-stdio", Args: args, Bootstrap: config.BootstrapSpec{Mode: "jsonrpc-stdio"}, Env: config.ProviderEnv{Mode: "merge"}}
		agent := config.Agent{ID: agentID, Name: r.Name, Model: r.Model, Roles: []string{r.Role}, Body: body, Permissions: config.AgentPermissions{PermissionMode: r.PermissionMode}}
		launch := config.Launch{ID: r.Name, Project: r.Project, Agent: agentID, Provider: providerID, Workspace: config.LaunchWorkspace{Mode: "shared", WriteHome: filepath.Join(root, "workspaces", r.Name)}, Overrides: config.LaunchOverrides{Env: env}, MCP: config.MCPConfig{Servers: r.MCPServers}}
		for _, path := range sortedRoleKeys(roleFiles) {
			if path == "AGENTS.md" {
				continue // Tether owns the generated provider instruction file.
			}
			f := roleFiles[path]
			launch.Injection.NativeFiles = append(launch.Injection.NativeFiles, config.InjectedFile{Kind: "raw", RelPath: filepath.ToSlash(path), Content: string(f.data), Mode: uint32(f.mode)})
		}
		profile := bootgen.Profile{ID: r.Name, DisplayName: r.Name, Launch: r.Name, MCPServers: r.MCPServers, MCPTools: r.MCPTools, Template: filepath.Join(root, "assets", r.Name, "boot.tmpl"), Identity: bootgen.Identity{Role: r.Role, Project: r.Project, WorkRoot: r.Scope, ProfileID: r.Name, ProfileVersion: 1}, Slots: map[string]bootgen.SlotSource{"agent": {Type: "static", Path: filepath.Join(root, "assets", r.Name, "AGENTS.md")}, "context": {Type: "static", Path: filepath.Join(root, "assets", r.Name, "prompt.md")}}}
		files[filepath.Join("assets", r.Name, "AGENTS.md")] = []byte(body)
		files[filepath.Join("assets", r.Name, "prompt.md")] = []byte("RUN CONTEXT: runtime-managed\n\n" + r.Prompt)
		files[filepath.Join("assets", r.Name, "boot.tmpl")] = []byte("{{ slot \"agent\" }}\n\n{{ slot \"context\" }}\n")
		for name, value := range map[string]any{filepath.Join("providers", providerID+".yaml"): provider, filepath.Join("agents", agentID+".yaml"): agent, filepath.Join("launches", r.Name+".yaml"): launch} {
			if err := put(name, value); err != nil {
				return nil, err
			}
		}
		// Profile.MarshalYAML would omit [] due to omitempty; explicit empty
		// grant arrays must survive the conversion and never become inheritance.
		b, err := yaml.Marshal(profile)
		if err != nil {
			return nil, err
		}
		var node map[string]any
		if err := yaml.Unmarshal(b, &node); err != nil {
			return nil, err
		}
		node["mcp_servers"], node["mcp_tools"] = r.MCPServers, r.MCPTools
		if err := put(filepath.Join("boot-profiles", r.Name+".yaml"), node); err != nil {
			return nil, err
		}
	}
	if len(usedServers) != len(servers) {
		return nil, errors.New("unused MCP definitions are refused; import only the roles' baseline servers")
	}
	for id, scope := range projects {
		// Legacy resolution gives an explicit project list priority over the
		// launch. Leave it unset so each role's explicit (possibly empty)
		// launch list is authoritative; never put a shared grant on a project.
		p := config.Project{ID: id, Name: id, RepoRoot: scope, TrackingRoot: m.TeamHome, Workspace: config.WorkspaceSpec{DefaultMode: "shared", SessionRoot: filepath.Join(root, "workspaces", id)}}
		if err := put(filepath.Join("projects", id+".yaml"), p); err != nil {
			return nil, err
		}
	}
	g := config.Global{Version: "1", Identity: config.IdentityConfig{Mode: "enforce"}, Daemon: config.DaemonConfig{ListenAddr: "unix://" + filepath.Join(root, "control", "tetherd.sock"), PIDFile: filepath.Join(root, "control", "tetherd.pid")}, Catalog: config.CatalogRoots{Defaults: config.Defaults{StateDB: filepath.Join(root, "state", "tether.db"), WorkspaceRoot: filepath.Join(root, "workspaces"), TempRoot: filepath.Join(root, "tmp")}}}
	if err := put("global.yaml", g); err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	files["baseline.json"] = manifest
	return files, nil
}

func directory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("source and scope paths must be absolute existing directories")
	}
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return errors.New("source and scope paths must be absolute existing directories")
	}
	return nil
}

func beneath(parent, child string) bool {
	r, err := filepath.Rel(parent, child)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func inlineTable(env map[string]string) string {
	items := []string{}
	for _, k := range sortedKeys(env) {
		items = append(items, strconv.Quote(k)+"="+strconv.Quote(env[k]))
	}
	return "{" + strings.Join(items, ",") + "}"
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type roleFile struct {
	data []byte
	mode fs.FileMode
}

func sortedRoleKeys(m map[string]roleFile) []string { return sortedKeys(m) }

// Only instructions and the complete skill tree are imported. Provider config,
// launch scripts, histories, hooks and auth.json are not copied or executed.
func readRoleFiles(root string) (map[string]roleFile, error) {
	files := map[string]roleFile{}
	var total int64
	read := func(path, rel string, d fs.DirEntry) error {
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return errors.New("role assets must be regular files; symlinks and special files are refused")
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		total += st.Size()
		if st.Size() > 4<<20 || total > 16<<20 {
			return errors.New("role assets exceed import size limit")
		}
		base := strings.ToLower(d.Name())
		if base == "auth.json" || strings.HasPrefix(base, ".env") || strings.Contains(base, "credential") || strings.HasSuffix(base, ".token") {
			return errors.New("credential-like role asset names are refused")
		}
		b, err := os.ReadFile(path) //nolint:gosec // Validated regular instruction/skill file within explicit prepared boot source.
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return errors.New("binary role assets cannot be represented by supported text injection; conversion refused")
		}
		files[rel] = roleFile{data: b, mode: st.Mode().Perm() & 0o755}
		return nil
	}
	st, err := os.Lstat(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		return nil, errors.New("prepared Cairn boot must contain AGENTS.md")
	}
	if err := read(filepath.Join(root, "AGENTS.md"), "AGENTS.md", fs.FileInfoToDirEntry(st)); err != nil {
		return nil, err
	}
	skillRoot := filepath.Join(root, ".agents", "skills")
	for _, path := range []string{filepath.Join(root, ".agents"), skillRoot} {
		st, err := os.Lstat(path)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("skill directory symlinks are refused")
		}
	}
	if _, err := os.Lstat(skillRoot); errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	if err := filepath.WalkDir(skillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return read(path, rel, d)
	}); err != nil {
		return nil, err
	}
	return files, nil
}

func nonSecretMCP(e config.MCPServerEntry) error {
	ref := func(s string) bool {
		return strings.HasPrefix(s, "file://") || strings.HasPrefix(s, "helper://") || strings.HasPrefix(s, "keychain://")
	}
	if e.Token != "" && !ref(e.Token) {
		return errors.New("MCP literal credentials are refused; use an existing private file or helper reference")
	}
	if e.URL != "" {
		u, err := url.Parse(e.URL)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("MCP URLs must not contain userinfo, query credentials or fragments")
		}
	}
	if e.TokenFile != "" && !filepath.IsAbs(e.TokenFile) {
		return errors.New("MCP token_file must explicitly name an existing absolute path; it is not resolved or copied")
	}
	for _, arg := range e.Args {
		if arg == "--token" || arg == "-token" || strings.HasPrefix(arg, "--token=") || strings.HasPrefix(arg, "-token=") || strings.Contains(strings.ToLower(arg), "api-key") {
			return errors.New("MCP token argv is refused; use token_file")
		}
	}
	for key, value := range e.Env {
		k := strings.ToLower(key)
		if (strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "api_key")) && !ref(value) {
			return errors.New("MCP literal credential environment is refused")
		}
	}
	if e.Transport != "stdio" && e.Transport != "http" && e.Transport != "sse" {
		return errors.New("MCP transport must be explicit")
	}
	if e.Transport == "stdio" && e.Command == "" {
		return errors.New("stdio MCP command must be explicit")
	}
	return nil
}
