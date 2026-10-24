package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// fakeRealHome builds a home with ordinary entries, a .gemini that holds login
// files and an operator settings.json, and a .agents directory.
func fakeRealHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, f := range []string{".ssh/config", ".zshrc", "dev/notes.txt", ".agents/skills/x/SKILL.md", ".DS_Store"} {
		writeFile(t, filepath.Join(home, f), "x", 0o600)
	}
	for _, f := range []string{"oauth_creds.json", "google_accounts.json", "installation_id", "antigravity-cli/antigravity-oauth-token"} {
		writeFile(t, filepath.Join(home, ".gemini", f), "secret-"+f, 0o600)
	}
	writeFile(t, filepath.Join(home, ".gemini/antigravity-cli/settings.json"), `{"model":"operator-model","permissions":{"allow":["mcp(operator/keep)"]}}`, 0o600)
	return home
}

// snapshot lists every path under root with its size and symlink target, so a
// test can prove the real home was not written.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // an entry that vanished mid-walk is simply not in the snapshot
		}
		line := p + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			raw, _ := os.ReadFile(p)
			line += " " + string(raw)
		}
		out = append(out, line)
		return nil
	})
	sort.Strings(out)
	return out
}

func readAllow(t *testing.T, dir string) (map[string]any, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".gemini/antigravity-cli/settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var allow []string
	if perms, ok := doc["permissions"].(map[string]any); ok {
		for _, r := range perms["allow"].([]any) {
			allow = append(allow, r.(string))
		}
	}
	return doc, allow
}

func TestPluginServersReadsThePlantedConfig(t *testing.T) {
	boot := t.TempDir()
	if _, err := PluginServers(boot); err == nil {
		t.Fatal("a missing planted config must be an error, not an empty allowlist")
	}
	writeFile(t, filepath.Join(boot, ".agents/plugins/tether/mcp_config.json"), `{"mcpServers":{"zeta":{"command":"z"},"tether":{"command":"t"}}}`, 0o600)
	got, err := PluginServers(boot)
	if err != nil || !reflect.DeepEqual(got, []string{"tether", "zeta"}) {
		t.Fatalf("servers=%v err=%v", got, err)
	}
	writeFile(t, filepath.Join(boot, ".agents/plugins/tether/mcp_config.json"), `not json`, 0o600)
	if _, err := PluginServers(boot); err == nil {
		t.Fatal("malformed config must be an error")
	}
}

func TestAllowRulesGrantOnlyTethersOwnServer(t *testing.T) {
	rules, denied := AllowRules([]string{"zeta", "tether", "torque"})
	if !reflect.DeepEqual(rules, []string{"mcp(tether_tether/*)"}) {
		t.Fatalf("rules=%v", rules)
	}
	if !reflect.DeepEqual(denied, []string{"torque", "zeta"}) {
		t.Fatalf("denied=%v", denied)
	}
	if r, d := AllowRules(nil); len(r) != 0 || len(d) != 0 {
		t.Fatalf("no servers must mean no rules: %v %v", r, d)
	}
}

func TestPlantHomeBuildsTheFarmAndNeverWritesTheRealHome(t *testing.T) {
	realHome := fakeRealHome(t)
	before := snapshot(t, realHome)
	dir := filepath.Join(t.TempDir(), HomeDirName)

	got, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir, Rules: []string{"mcp(tether_tether/*)"}})
	if err != nil || got != dir {
		t.Fatalf("dir=%q err=%v", got, err)
	}

	for _, name := range []string{".ssh", ".zshrc", "dev"} {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || target != filepath.Join(realHome, name) {
			t.Fatalf("%s should link to the realHome entry, got %q (%v)", name, target, err)
		}
	}
	for _, name := range []string{".agents", ".DS_Store"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s must not be in the farm", name)
		}
	}
	info, err := os.Lstat(filepath.Join(dir, ".gemini"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".gemini must be a private realHome directory: %v %v", info, err)
	}
	for _, rel := range []string{"oauth_creds.json", "google_accounts.json", "installation_id", "antigravity-cli/antigravity-oauth-token"} {
		target, err := os.Readlink(filepath.Join(dir, ".gemini", rel))
		if err != nil || target != filepath.Join(realHome, ".gemini", rel) {
			t.Fatalf("login file %s should link to the realHome one, got %q (%v)", rel, target, err)
		}
	}
	// A login file the realHome home does not have is not invented.
	if _, err := os.Lstat(filepath.Join(dir, ".gemini/jetski-standalone-oauth-token")); err == nil {
		t.Fatal("linked a login file that does not exist")
	}

	doc, allow := readAllow(t, dir)
	if !reflect.DeepEqual(allow, []string{"mcp(tether_tether/*)"}) {
		t.Fatalf("allow=%v", allow)
	}
	if _, has := doc["model"]; has {
		t.Fatal("the operator's settings must not be copied into the private one")
	}
	st, _ := os.Stat(filepath.Join(dir, ".gemini/antigravity-cli/settings.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode %v, want 0600", st.Mode().Perm())
	}

	if after := snapshot(t, realHome); !reflect.DeepEqual(before, after) {
		t.Fatalf("the realHome home was changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPlantHomeMergesKeepsForeignRulesAndDropsRevokedOnes(t *testing.T) {
	realHome := fakeRealHome(t)
	dir := filepath.Join(t.TempDir(), HomeDirName)
	// Something else already wrote a key and a rule into the private settings.
	writeFile(t, filepath.Join(dir, ".gemini/antigravity-cli/settings.json"),
		`{"trustedWorkspaces":["/w"],"permissions":{"allow":["mcp(other/keep)"],"deny":["x"]}}`, 0o600)

	plant := func(rules ...string) []string {
		t.Helper()
		if _, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir, Rules: rules}); err != nil {
			t.Fatal(err)
		}
		_, allow := readAllow(t, dir)
		return allow
	}
	if got := plant("mcp(tether_tether/*)", "mcp(tether_zeta/ok)"); !slices.Equal(got, []string{"mcp(other/keep)", "mcp(tether_tether/*)", "mcp(tether_zeta/ok)"}) {
		t.Fatalf("first plant: %v", got)
	}
	doc, _ := readAllow(t, dir)
	if doc["trustedWorkspaces"] == nil || doc["permissions"].(map[string]any)["deny"] == nil {
		t.Fatalf("unrelated keys were lost: %v", doc)
	}
	// The same plant again changes nothing (no duplicate rules).
	if got := plant("mcp(tether_tether/*)", "mcp(tether_zeta/ok)"); len(got) != 3 {
		t.Fatalf("a repeat plant duplicated rules: %v", got)
	}
	// A revoked grant disappears at the next plant; a foreign rule stays.
	if got := plant("mcp(tether_tether/*)"); !slices.Equal(got, []string{"mcp(other/keep)", "mcp(tether_tether/*)"}) {
		t.Fatalf("revoked rule still present: %v", got)
	}
	if got := plant(); !slices.Equal(got, []string{"mcp(other/keep)"}) {
		t.Fatalf("revoking everything must leave only foreign rules: %v", got)
	}
}

func TestPlantHomeRefusesUnsafeTargets(t *testing.T) {
	realHome := fakeRealHome(t)
	for name, dir := range map[string]string{
		"the realHome home":       realHome,
		"the realHome .gemini":    filepath.Join(realHome, ".gemini"),
		"inside realHome .gemini": filepath.Join(realHome, ".gemini", "farm"),
		"inside realHome .agents": filepath.Join(realHome, ".agents", "farm"),
		"a relative directory":    "relative/farm",
	} {
		if _, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir}); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if _, err := os.Stat(filepath.Join(realHome, ".gemini", "farm")); err == nil {
		t.Fatal("a refused plant still created a directory under the realHome .gemini")
	}

	// A pre-planted symlink at the private .gemini must not be written through.
	dir := filepath.Join(t.TempDir(), HomeDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realHome, ".gemini"), filepath.Join(dir, ".gemini")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, realHome)
	if _, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir, Rules: []string{"mcp(x/y)"}}); err == nil {
		t.Fatal("must refuse to write through a symlinked .gemini")
	}
	if after := snapshot(t, realHome); !reflect.DeepEqual(before, after) {
		t.Fatal("the realHome .gemini was modified through the symlink")
	}
}

func TestPlantHomeRemovesLinksToEntriesThatNoLongerExist(t *testing.T) {
	realHome := fakeRealHome(t)
	dir := filepath.Join(t.TempDir(), HomeDirName)
	if _, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(realHome, "dev")); err != nil {
		t.Fatal(err)
	}
	if _, err := PlantHome(HomeSpec{RealHome: realHome, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev")); err == nil {
		t.Fatal("the dangling link to a removed entry should be gone")
	}
}

func TestWithHomeReplacesEveryHOME(t *testing.T) {
	got := WithHome([]string{"A=1", "HOME=/old", "PATH=/p", "HOME=/older", "HOMEX=keep"}, "/farm")
	want := []string{"A=1", "PATH=/p", "HOMEX=keep", "HOME=/farm"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := WithHome(nil, "/farm"); !slices.Equal(got, []string{"HOME=/farm"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAllowlistIsOffUnlessExplicitlyOn(t *testing.T) {
	for val, want := range map[string]bool{"": false, "0": false, "true": false, "1": true, " 1 ": true} {
		if got := AllowlistEnabled(func(string) string { return val }); got != want {
			t.Errorf("%q: got %v want %v", val, got, want)
		}
	}
	_ = strings.TrimSpace
}
