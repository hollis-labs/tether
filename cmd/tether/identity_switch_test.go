package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
)

func switchFixture(t *testing.T, identityYAML string) (root, logPath string, mode func(string) string, rollback func(point string, observe bool)) {
	t.Helper()
	state := t.TempDir()
	root = filepath.Join(state, "catalog")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	global := "# keep me\ndaemon:\n  listen_addr: unix:/tmp/x.sock\n" + identityYAML
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath = flipLogPath(root)
	mode = func(point string) string {
		cat, err := config.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		if point == "global" {
			return cat.Global.Identity.EffectiveMode()
		}
		return cat.Global.Identity.ModeFor(point)
	}
	rollback = func(point string, observe bool) {
		t.Helper()
		recs, _, err := verifyFlipLog(logPath)
		if err != nil {
			t.Fatal(err)
		}
		targets := []string{point}
		if point == "" {
			targets = append([]string{"global"}, config.IdentityPoints...)
		}
		pending := pendingFlips(recs)
		for _, p := range targets {
			switch {
			case observe:
				if _, err := applyModeChange(root, "reset", p, "observe", "t", "x"); err != nil {
					t.Fatal(err)
				}
			case len(pending[p]) > 0:
				if _, err := applyModeChange(root, "rollback", p, pending[p][len(pending[p])-1].Prior, "t", "x"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return
}

func TestIdentityFlipAndRollbackAreAudited(t *testing.T) {
	root, logPath, mode, rollback := switchFixture(t, "identity:\n  mode: observe\n")
	if _, err := applyModeChange(root, "flip", "bogus", "enforce", "t", "e"); err == nil {
		t.Fatal("unknown point accepted")
	}
	if _, err := applyModeChange(root, "flip", "teams", "yes", "t", "e"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	rec, err := applyModeChange(root, "flip", "mcp_gateway", "enforce", "chris", "CW-1 GO")
	if err != nil || rec.From != "observe" || rec.To != "enforce" || rec.Prior != "" {
		t.Fatalf("flip: %v %+v", err, rec)
	}
	if mode("mcp_gateway") != "enforce" || mode("teams") != "observe" || mode("local_api") != "observe" {
		t.Fatal("flip was not scoped to one point")
	}
	cat, _ := config.Load(root)
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil || cfg.IdentityModeFor("mcp_gateway") != identity.Enforce || cfg.IdentityModeFor("local_api") != identity.Observe {
		t.Fatalf("daemon config did not resolve per-point mode: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "global.yaml")); !strings.Contains(string(b), "# keep me") {
		t.Fatal("comment lost")
	}
	if _, err := applyModeChange(root, "flip", "global", "enforce", "chris", "CW-1 GO"); err != nil {
		t.Fatal(err)
	}
	if mode("local_api") != "enforce" {
		t.Fatal("global flip not effective")
	}
	rollback("", false)
	for _, p := range append([]string{"global"}, config.IdentityPoints...) {
		if mode(p) != "observe" {
			t.Fatalf("%s is %s after rollback", p, mode(p))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "global.yaml")); strings.Contains(string(b), "mcp_gateway") {
		t.Fatal("rollback left an override that did not exist before the flip")
	}
	recs, head, err := verifyFlipLog(logPath)
	if err != nil || len(recs) != 4 || recs[0].Evidence != "CW-1 GO" || recs[0].By != "chris" || head != recs[3].Hash {
		t.Fatalf("audit log: %v %+v", err, recs)
	}
}

func TestIdentityRollbackRestoresPriorStateOnly(t *testing.T) {
	root, _, mode, rollback := switchFixture(t, "identity:\n  mode: enforce\n  points:\n    teams: enforce\n    local_api: observe\n")
	if _, err := applyModeChange(root, "flip", "local_api", "enforce", "t", "e"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyModeChange(root, "flip", "mcp_gateway", "observe", "t", "e"); err != nil {
		t.Fatal(err)
	}
	rollback("", false)
	if mode("local_api") != "observe" || mode("mcp_gateway") != "enforce" || mode("teams") != "enforce" || mode("global") != "enforce" {
		t.Fatalf("rollback did not restore prior state: local_api=%s mcp=%s teams=%s global=%s", mode("local_api"), mode("mcp_gateway"), mode("teams"), mode("global"))
	}
	if b, _ := os.ReadFile(filepath.Join(root, "global.yaml")); strings.Contains(string(b), "mcp_gateway") {
		t.Fatal("inherited point stayed pinned")
	}
	// Break-glass forces explicit observe everywhere.
	rollback("", true)
	if mode("global") != "observe" || mode("teams") != "observe" {
		t.Fatal("--observe did not force observe")
	}
}

func TestIdentityFlipLogIsTamperEvident(t *testing.T) {
	root, logPath, _, _ := switchFixture(t, "identity:\n  mode: observe\n")
	for _, p := range []string{"teams", "local_api", "mcp_gateway"} {
		if _, err := applyModeChange(root, "flip", p, "enforce", "t", "e"); err != nil {
			t.Fatal(err)
		}
	}
	good, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(good)), "\n")
	cases := map[string]string{
		"altered by":     strings.Replace(string(good), `"by":"t"`, `"by":"mallory"`, 1),
		"removed middle": lines[0] + "\n" + lines[2] + "\n",
		"malformed":      lines[0] + "\n{not json\n",
		"blank":          lines[0] + "\n\n" + lines[1] + "\n",
		"swapped":        lines[1] + "\n" + lines[0] + "\n" + lines[2] + "\n",
	}
	for name, body := range cases {
		if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := verifyFlipLog(logPath); err == nil {
			t.Errorf("%s: tampering not detected", name)
		}
		if _, err := applyModeChange(root, "flip", "teams", "observe", "t", "e"); err == nil {
			t.Errorf("%s: appended to a broken log", name)
		}
	}
	// Tail truncation verifies, but the head hash differs from the pinned one.
	if err := os.WriteFile(logPath, []byte(lines[0]+"\n"+lines[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, head, err := verifyFlipLog(logPath); err != nil || head == lastHash(t, lines[2]) {
		t.Fatalf("truncation: err=%v", err)
	}
}

func lastHash(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, `"hash":"`)
	return strings.TrimSuffix(line[i+len(`"hash":"`):], `"}`)
}

func TestIdentityVerbsRefuseInsideSession(t *testing.T) {
	for _, k := range []string{"TETHER_MCP_TOKEN", "TETHER_TOKEN"} {
		t.Setenv("TETHER_MCP_TOKEN", "")
		t.Setenv("TETHER_TOKEN", "")
		if err := refuseInsideSession(); err != nil {
			t.Fatal("plain shell refused")
		}
		t.Setenv(k, "tether-worker")
		if err := refuseInsideSession(); err == nil {
			t.Fatalf("%s set but not refused", k)
		}
	}
	root, _, _, _ := switchFixture(t, "identity:\n  mode: observe\n")
	t.Setenv("TETHER_MCP_TOKEN", "tether-worker")
	cmd := identityFlipCmd()
	cmd.SetArgs([]string{"--point", "teams", "--to", "enforce", "--evidence", "x"})
	old := catalogPath
	catalogPath = root
	defer func() { catalogPath = old }()
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("flip verb ran inside a session: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "global.yaml")); strings.Contains(string(b), "teams") {
		t.Fatal("config changed")
	}
}

func TestDoctorFailsLoudlyOnBrokenFlipLog(t *testing.T) {
	root, logPath, _, _ := switchFixture(t, "identity:\n  mode: observe\n")
	if _, err := applyModeChange(root, "flip", "teams", "enforce", "t", "e"); err != nil {
		t.Fatal(err)
	}
	old := catalogPath
	catalogPath = root
	defer func() { catalogPath = old }()
	cat, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if r := checkIdentity(cat); r.Status == "fail" {
		t.Fatalf("intact log failed doctor: %+v", r)
	}
	b, _ := os.ReadFile(logPath)
	if err := os.WriteFile(logPath, []byte(strings.Replace(string(b), `"to":"enforce"`, `"to":"observe"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkIdentity(cat); r.Status != "fail" || !strings.Contains(r.Message, "integrity") {
		t.Fatalf("doctor accepted a forged log: %+v", r)
	}
}
