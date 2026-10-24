package antigravity

// Per-launch MCP allowlist for headless agy (CW-20261024-0111).
//
// A headless `agy -p` run has nobody to answer a permission prompt, so any MCP
// tool call without an allow rule is auto-denied: the run exits 0 with an empty
// response. The only place agy honors a rule is permissions.allow in
// ~/.gemini/antigravity-cli/settings.json, resolved from $HOME. Writing the
// operator's real file would be global and shared with the desktop app, so each
// launch gets its own HOME: a "farm" of symlinks to the real home's entries
// except .gemini and .agents, which are private to the launch. The private
// .gemini links the operator's login files and carries a generated settings.json.
//
// What was verified live (macOS and Linux, agy 1.4.7, 2026-10-24): an allowed
// tool runs, a tool without a rule is denied, the real ~/.gemini is not edited,
// and agy still authenticates under the relocated HOME. Hooks cannot allow a
// call, and --dangerously-skip-permissions is not per-tool, so neither is used.
//
// Known limits: the login files are symlinks, so a token refresh by the agent
// rewrites the operator's real token file (observed on Linux). Non-MCP tools
// (commands, file writes, URL reads) stay denied headless. The rule syntax and
// settings path are observed behavior of agy, not a documented API.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	// AllowlistEnv turns the per-launch allowlist on when set to "1". It is off
	// by default: enabling it relocates HOME for every agy launch.
	AllowlistEnv = "TETHER_AGY_MCP_ALLOWLIST"

	// PluginName is the plugin the shared planter writes (.agents/plugins/tether).
	// agy names a plugin's MCP server <plugin>_<server>.
	PluginName = "tether"

	// GatewayServer is the planter's name for Tether's own MCP server. It is the
	// one server a launch is allowed to call: its tool set is already limited
	// server-side by the agent's profile, so the rule is server-wide.
	GatewayServer = "tether"

	// HomeDirName is the directory, under a session workspace, that holds the
	// launch's private HOME.
	HomeDirName = "agy-home"

	ownedRulesFile = "tether-owned-rules.json"
)

// AllowlistEnabled reports whether the allowlist is on.
func AllowlistEnabled(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(AllowlistEnv)) == "1"
}

// farmExcluded are the real-home entries a farm never links: the two directories
// that are private to the launch, and macOS litter.
var farmExcluded = map[string]bool{
	".gemini": true, ".agents": true, ".DS_Store": true, ".CFUserTextEncoding": true, ".Trash": true,
}

// authFiles are the login files linked into the private .gemini, relative to it.
// Only the ones that exist in the real home are linked; Linux has fewer.
var authFiles = []string{
	"oauth_creds.json",
	"google_accounts.json",
	"jetski-standalone-oauth-token",
	"installation_id",
	filepath.Join("antigravity-cli", "antigravity-oauth-token"),
	filepath.Join("antigravity-cli", "installation_id"),
}

// PluginServers returns the MCP server names the planter wrote into the launch's
// plugin, sorted. The planted file is the authority on what agy will see.
func PluginServers(bootDir string) ([]string, error) {
	path := filepath.Join(bootDir, ".agents", "plugins", PluginName, "mcp_config.json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is built from the planted boot dir
	if err != nil {
		return nil, fmt.Errorf("agy allowlist: read planted plugin config: %w", err)
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("agy allowlist: parse %s: %w", path, err)
	}
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ServerName is the name agy reports for a plugin server.
func ServerName(server string) string { return PluginName + "_" + server }

// AllowRules returns the allow rules for the planted servers and the servers
// that get none. Only Tether's own server is granted, server-wide; any other
// server in the plugin stays denied until a grant policy names it.
func AllowRules(servers []string) (rules, denied []string) {
	for _, s := range servers {
		if s == GatewayServer {
			rules = append(rules, "mcp("+ServerName(s)+"/*)")
			continue
		}
		denied = append(denied, s)
	}
	sort.Strings(rules)
	sort.Strings(denied)
	return rules, denied
}

// HomeSpec describes one launch's private HOME.
type HomeSpec struct {
	// RealHome is the home whose entries the farm links (never written to).
	RealHome string
	// Dir is the farm directory, which becomes the launch's HOME.
	Dir string
	// Rules are the allow rules to own in settings.json.
	Rules []string
}

// PlantHome builds the farm and the private .gemini and merges the rules into
// its settings.json. It never writes under RealHome, and it is safe to run again
// on the same Dir: links are reconciled, unrelated settings keys and foreign
// allow rules are kept, and rules it wrote earlier that are no longer granted are
// dropped.
func PlantHome(spec HomeSpec) (string, error) {
	realHome, dir := filepath.Clean(spec.RealHome), filepath.Clean(spec.Dir)
	if !filepath.IsAbs(realHome) || !filepath.IsAbs(dir) {
		return "", errors.New("agy allowlist: real home and farm dir must be absolute")
	}
	if dir == realHome || dir == filepath.Join(realHome, ".gemini") || within(dir, filepath.Join(realHome, ".gemini")) || within(dir, filepath.Join(realHome, ".agents")) {
		return "", fmt.Errorf("agy allowlist: farm dir %s must not be the real home or inside the real .gemini or .agents", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("agy allowlist: %w", err)
	}
	if err := linkFarm(realHome, dir); err != nil {
		return "", err
	}
	gemini := filepath.Join(dir, ".gemini")
	if info, err := os.Lstat(gemini); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("agy allowlist: %s is a symlink; refusing to write through it", gemini)
	}
	cli := filepath.Join(gemini, "antigravity-cli")
	for _, d := range []string{cli, filepath.Join(gemini, "config")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("agy allowlist: %w", err)
		}
	}
	for _, rel := range authFiles {
		if err := linkIfPresent(filepath.Join(realHome, ".gemini", rel), filepath.Join(gemini, rel)); err != nil {
			return "", err
		}
	}
	if err := mergeSettings(cli, spec.Rules); err != nil {
		return "", err
	}
	return dir, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// linkFarm makes dir hold one symlink per entry of the real home, except the
// excluded ones, and removes links it made earlier whose target is gone.
func linkFarm(realHome, dir string) error {
	entries, err := os.ReadDir(realHome)
	if err != nil {
		return fmt.Errorf("agy allowlist: read real home: %w", err)
	}
	want := map[string]bool{}
	for _, e := range entries {
		if farmExcluded[e.Name()] {
			continue
		}
		want[e.Name()] = true
		if err := linkIfPresent(filepath.Join(realHome, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	existing, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	for _, e := range existing {
		if want[e.Name()] || farmExcluded[e.Name()] {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if target, err := os.Readlink(path); err == nil && within(target, realHome) {
			_ = os.Remove(path) // a link we made to an entry that no longer exists
		}
	}
	return nil
}

// linkIfPresent symlinks src at dst when src exists. An existing correct link is
// left alone, a wrong link is replaced, and a real file or directory is never
// touched.
func linkIfPresent(src, dst string) error {
	if _, err := os.Lstat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("agy allowlist: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if cur, err := os.Readlink(dst); err == nil && cur == src {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("agy allowlist: %w", err)
		}
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	return nil
}

// mergeSettings writes permissions.allow into cli/settings.json: keys and allow
// rules it does not own are kept, rules it wrote before are replaced by rules.
func mergeSettings(cli string, rules []string) error {
	settingsPath := filepath.Join(cli, "settings.json")
	sidecarPath := filepath.Join(cli, ownedRulesFile)

	doc := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil { //nolint:gosec // G304: private launch dir
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("agy allowlist: %s is not valid JSON: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	var owned []string
	if raw, err := os.ReadFile(sidecarPath); err == nil { //nolint:gosec // G304: private launch dir
		_ = json.Unmarshal(raw, &owned)
	}

	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	var keep []any
	if cur, ok := perms["allow"].([]any); ok {
		for _, r := range cur {
			if s, ok := r.(string); ok && (slices.Contains(owned, s) || slices.Contains(rules, s)) {
				continue
			}
			keep = append(keep, r)
		}
	}
	for _, r := range rules {
		keep = append(keep, r)
	}
	if keep == nil {
		keep = []any{}
	}
	perms["allow"] = keep
	doc["permissions"] = perms

	if err := writeJSONFile(settingsPath, doc); err != nil {
		return err
	}
	if rules == nil {
		rules = []string{}
	}
	return writeJSONFile(sidecarPath, rules)
}

// writeJSONFile writes v as indented JSON at path, mode 0600, through a rename
// so a reader never sees half a file.
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("agy allowlist: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("agy allowlist: %w", err)
	}
	return nil
}

// WithHome returns env with HOME set to home, replacing every HOME entry.
func WithHome(env []string, home string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HOME="+home)
}
