package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func healthFor(goos string, env map[string]string, probeErr error) app.ProtectionHealth {
	return app.ComputeProtectionHealth(app.ControlPlaneProtection(goos, envOf(env)), goos, "/catalog",
		func(string) error { return probeErr })
}

// The daemon logs its protection decision at startup. Turned off, by the
// operator or on a platform not yet covered, or on but unusable because
// bubblewrap cannot build a namespace, it is a WARN line naming why.
func TestLogControlPlaneProtection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health app.ProtectionHealth
		want   string
		warn   bool
	}{
		{"on", healthFor("linux", nil, nil), "control-plane protection on", false},
		{"switch off", healthFor("linux", map[string]string{app.ProtectEnv: "0"}, nil), "WARN: control-plane protection DISABLED by TETHER_SANDBOX_PROTECT=0, so agents can write the catalog, run/ and state/", true},
		{"darwin", healthFor("darwin", nil, nil), "WARN: control-plane protection not applied on darwin", true},
		{"on but unusable", healthFor("linux", nil, fmt.Errorf("bwrap: setting up uid map: Permission denied")), "WARN: control-plane protection is on but unusable: bwrap: setting up uid map: Permission denied", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			logControlPlaneProtection(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, tc.health)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.want) || strings.HasPrefix(lines[0], "WARN") != tc.warn {
				t.Fatalf("logged %q; want one line starting %q", lines, tc.want)
			}
			if tc.name == "on but unusable" && !strings.Contains(lines[0], "Codex launches are not affected") {
				t.Fatalf("unusable warning does not say Codex launches are unaffected: %q", lines[0])
			}
		})
	}
}

// tether doctor reports what the daemon says, since the daemon's environment
// decides protection. It warns when protection is off, and fails when it is on
// but bubblewrap cannot build a namespace, since every launch Tether wraps (not Codex's)
// is then refused. Without a daemon it says its answer is its own shell's.
func TestCheckSandboxProtect(t *testing.T) {
	for _, tc := range []struct {
		name       string
		health     app.ProtectionHealth
		fromDaemon bool
		want       string
		message    string
	}{
		{"on", healthFor("linux", nil, nil), true, statusOK, "cannot write the catalog, run/ or state/"},
		{"on, unusable", healthFor("linux", nil, fmt.Errorf("bwrap: No permissions to create a new namespace")), true, statusFail, "Claude, OpenCode and every agent Tether wraps will be refused; Codex launches are not affected"},
		{"switch off", healthFor("linux", map[string]string{app.ProtectEnv: "false"}, nil), true, statusWarn, "DISABLED by TETHER_SANDBOX_PROTECT=false"},
		{"darwin", healthFor("darwin", nil, nil), true, statusWarn, "CW-20261001-0138"},
		{"no daemon", healthFor("linux", nil, nil), false, statusOK, "this shell's environment"},
		// A plan failure is a catalog problem, never blamed on bubblewrap, even
		// though the probe on its own found bubblewrap fine.
		{"plan error", withPlanError(healthFor("linux", nil, nil), "protect control plane: catalog root: no such file"), true, statusFail, "not bubblewrap"},
		{"skipped layers", withSkipped(healthFor("linux", nil, nil), deadProjects()...), true, statusWarn, "left out of protection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkSandboxProtect(sandboxProtectHealth(tc.health), tc.fromDaemon)
			if tc.name == "on, unusable" && !strings.Contains(r.Message, "No permissions to create a new namespace") {
				t.Fatalf("the failure does not carry bwrap's error: %q", r.Message)
			}
			if r.Name != "sandbox-protect" || r.Status != tc.want || !strings.Contains(r.Message, tc.message) {
				t.Fatalf("check = %+v; want %s with a message containing %q", r, tc.want, tc.message)
			}
			if tc.name == "plan error" && (!strings.Contains(r.Message, "no such file") || strings.Contains(r.Message, "install bubblewrap") || strings.Contains(r.Remedy, "install bubblewrap")) {
				t.Fatalf("a plan failure must carry its own reason and not send the operator to bubblewrap: %+v", r)
			}
			if tc.name == "skipped layers" && (!strings.Contains(r.Message, "chrispian") || !strings.Contains(r.Message, "/gone/chrispian does not exist") || !strings.Contains(r.Message, "2 project(s)")) {
				t.Fatalf("the warning must name each dead project, its path and why: %+v", r)
			}
			if tc.fromDaemon && !strings.Contains(r.Message, "(daemon)") {
				t.Fatalf("message does not say it is the daemon's answer: %q", r.Message)
			}
			if tc.want != statusOK && r.Name == "sandbox-protect" && tc.name != "darwin" && r.Remedy == "" {
				t.Fatalf("check = %+v; a warning or failure with a fix needs a remedy", r)
			}
		})
	}
}

// The daemon's /health carries the same fields the check reads.
func TestSandboxProtectHealth(t *testing.T) {
	h := sandboxProtectHealth(healthFor("linux", nil, fmt.Errorf("denied")))
	want := daemon.SandboxProtectHealth{Enabled: true, Reason: h.Reason, Codex: "not protected", CodexReason: h.CodexReason, BwrapChecked: true, BwrapUsable: false, BwrapError: "denied"}
	if !reflect.DeepEqual(*h, want) {
		t.Fatalf("health = %+v, want %+v", *h, want)
	}
}

func deadProjects() []config.SkippedProjectLayer {
	return []config.SkippedProjectLayer{
		{Project: "chrispian", RepoRoot: "/gone/chrispian", Reason: "does not exist"},
		{Project: "lnklst", RepoRoot: "/gone/lnklst", Reason: "does not exist"},
	}
}

func withPlanError(h app.ProtectionHealth, msg string) app.ProtectionHealth {
	h.PlanError = msg
	return h
}

func withSkipped(h app.ProtectionHealth, skipped ...config.SkippedProjectLayer) app.ProtectionHealth {
	h.SkippedProjectLayers = skipped
	return h
}

// The daemon's /health carries the skipped project layers and the plan error as
// their own fields, so a consumer never has to infer either from a bubblewrap one.
func TestSandboxProtectHealthCarriesSkippedLayersAndPlanError(t *testing.T) {
	h := sandboxProtectHealth(withPlanError(withSkipped(healthFor("linux", nil, nil), deadProjects()...), "boom"))
	want := []daemon.SkippedProjectLayer{
		{Project: "chrispian", RepoRoot: "/gone/chrispian", Reason: "does not exist"},
		{Project: "lnklst", RepoRoot: "/gone/lnklst", Reason: "does not exist"},
	}
	if !reflect.DeepEqual(h.SkippedProjectLayers, want) || h.PlanError != "boom" {
		t.Fatalf("health = %+v; want skipped %+v and plan error boom", *h, want)
	}
	if h.BwrapChecked != true || !h.BwrapUsable || h.BwrapError != "" {
		t.Fatalf("the bubblewrap probe must stay its own answer: %+v", *h)
	}
}

// Startup logs a plan failure as what it is, not as a bubblewrap problem.
func TestLogControlPlaneProtectionPlanError(t *testing.T) {
	var lines []string
	logControlPlaneProtection(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
		withPlanError(healthFor("linux", nil, nil), "protect control plane: catalog root: no such file"))
	if len(lines) != 1 || !strings.Contains(lines[0], "no such file") || !strings.Contains(lines[0], "not a bubblewrap problem") || strings.Contains(lines[0], app.ProtectEnv+"=0") {
		t.Fatalf("log = %q; want the real reason, not a bubblewrap hint", lines)
	}
}

// The daemon says how codex is protected at startup, naming CW-20261001-0230 in
// both states. The build ships codex as not protected, which is a WARN so it is
// never silent; the dormant guard, switched on, is an ordinary line.
func TestLogCodexProtection(t *testing.T) {
	logged := func(h app.ProtectionHealth) []string {
		var lines []string
		logCodexProtection(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, h)
		return lines
	}
	shipped := healthFor("linux", nil, nil)
	if got := logged(shipped); len(got) != 1 || !strings.HasPrefix(got[0], "WARN: codex protection not protected (CW-20261001-0230)") {
		t.Fatalf("shipped log = %q; want a WARN naming CW-20261001-0230", got)
	}
	guarded := shipped
	guarded.Codex = app.CodexProtectionState{State: "guarded", Reason: "guarded (dormant guard switched on): codex runs under its own sandbox (CW-20261001-0230)"}
	if got := logged(guarded); len(got) != 1 || !strings.HasPrefix(got[0], "codex protection guarded") || strings.HasPrefix(got[0], "WARN") || !strings.Contains(got[0], "CW-20261001-0230") {
		t.Fatalf("guarded log = %q", got)
	}
	// With control-plane protection off the main line already says so.
	if got := logged(healthFor("linux", map[string]string{app.ProtectEnv: "0"}, nil)); len(got) != 0 {
		t.Fatalf("protection off still logged a codex line: %q", got)
	}
}

// tether doctor reports codex's protection as the daemon says it: a warning that
// names CW-20261001-0230 when not protected (as the build ships), ok when
// guarded (the dormant guard switched on), and honest about a daemon that does
// not report it.
func TestCheckCodexProtection(t *testing.T) {
	shipped := sandboxProtectHealth(healthFor("linux", nil, nil))
	r := checkCodexProtection(shipped, true)
	if r.Name != "sandbox-protect-codex" || r.Status != statusWarn || !strings.Contains(r.Message, "not protected (CW-20261001-0230)") || !strings.Contains(r.Message, "(daemon)") || !strings.Contains(r.Remedy, "CW-20261001-0230") {
		t.Fatalf("shipped check = %+v; want a warning naming CW-20261001-0230", r)
	}

	guarded := *shipped
	guarded.Codex, guarded.CodexReason = "guarded", "guarded (dormant guard switched on): codex runs under its own sandbox (CW-20261001-0230)"
	r = checkCodexProtection(&guarded, true)
	if r.Status != statusOK || !strings.Contains(r.Message, "guarded") || !strings.Contains(r.Message, "(daemon)") || !strings.Contains(r.Message, "CW-20261001-0230") {
		t.Fatalf("guarded check = %+v", r)
	}

	r = checkCodexProtection(&daemon.SandboxProtectHealth{Enabled: true}, true)
	if r.Status != statusOK || !strings.Contains(r.Message, "not reported by the daemon") {
		t.Fatalf("a daemon that predates the report: %+v", r)
	}

	r = checkCodexProtection(shipped, false)
	if !strings.Contains(r.Message, "this shell's environment") {
		t.Fatalf("a local answer must say so: %+v", r)
	}
}
