package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
)

func agyBootDir(t *testing.T, servers string) string {
	t.Helper()
	boot := t.TempDir()
	path := filepath.Join(boot, ".agents", "plugins", "tether", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":`+servers+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return boot
}

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func agyFlagEnv(on string) func(string) string {
	return func(k string) string {
		if k == antigravity.AllowlistEnv {
			return on
		}
		return ""
	}
}

func noHome() (string, error) { panic("the user home must not be consulted when env carries HOME") }

func TestAgyAllowlistLeavesEnvAloneWhenOffOrNotAgy(t *testing.T) {
	home := fakeHome(t)
	env := []string{"HOME=" + home, "PATH=/p"}
	boot := agyBootDir(t, `{"tether":{"command":"t"}}`)
	for name, tc := range map[string]struct{ brand, on string }{
		"off by default":  {"antigravity", ""},
		"explicit zero":   {"antigravity", "0"},
		"on but not agy":  {"claude", "1"},
		"on, codex brand": {"codex", "1"},
	} {
		ws := t.TempDir()
		got, err := withAgyAllowlist(tc.brand, boot, ws, env, agyFlagEnv(tc.on), noHome)
		if err != nil || !reflect.DeepEqual(got, env) {
			t.Errorf("%s: env changed or errored: %v %v", name, got, err)
		}
		if _, err := os.Stat(filepath.Join(ws, antigravity.HomeDirName)); err == nil {
			t.Errorf("%s: created a home directory", name)
		}
	}
}

func TestAgyAllowlistRelocatesHomeAndGrantsOnlyTether(t *testing.T) {
	home := fakeHome(t)
	ws := t.TempDir()
	boot := agyBootDir(t, `{"tether":{"command":"t"},"torque":{"command":"q"}}`)
	env := []string{"HOME=" + home, "PATH=/p"}

	got, err := withAgyAllowlist("antigravity", boot, ws, env, agyFlagEnv("1"), noHome)
	if err != nil {
		t.Fatal(err)
	}
	farm := filepath.Join(ws, antigravity.HomeDirName)
	if !reflect.DeepEqual(got, []string{"PATH=/p", "HOME=" + farm}) {
		t.Fatalf("env=%v", got)
	}
	raw, err := os.ReadFile(filepath.Join(farm, ".gemini", "antigravity-cli", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mcp(tether_tether/*)") || strings.Contains(string(raw), "torque") {
		t.Fatalf("only the Tether server may be granted: %s", raw)
	}
	if target, err := os.Readlink(filepath.Join(farm, ".zshrc")); err != nil || target != filepath.Join(home, ".zshrc") {
		t.Fatalf("farm should link the real home's entries: %q %v", target, err)
	}
	if target, err := os.Readlink(filepath.Join(farm, ".gemini", "antigravity-cli", "antigravity-oauth-token")); err != nil || target != filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token") {
		t.Fatalf("login token should link to the real one: %q %v", target, err)
	}
}

func TestAgyAllowlistFallsBackToTheUserHomeWhenEnvHasNone(t *testing.T) {
	home := fakeHome(t)
	ws := t.TempDir()
	boot := agyBootDir(t, `{"tether":{"command":"t"}}`)
	got, err := withAgyAllowlist("antigravity", boot, ws, []string{"PATH=/p"}, agyFlagEnv("1"), func() (string, error) { return home, nil })
	if err != nil || got[len(got)-1] != "HOME="+filepath.Join(ws, antigravity.HomeDirName) {
		t.Fatalf("env=%v err=%v", got, err)
	}
}

func TestAgyAllowlistFailsTheLaunchWhenEnabledButUnbuildable(t *testing.T) {
	home := fakeHome(t)
	// No planted plugin config: enabled means the operator wants it, so say so.
	if _, err := withAgyAllowlist("antigravity", t.TempDir(), t.TempDir(), []string{"HOME=" + home}, agyFlagEnv("1"), noHome); err == nil {
		t.Fatal("a missing planted config must fail the launch, not silently keep a HOME that denies every call")
	}
}
