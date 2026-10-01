package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
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
		{"switch off", healthFor("linux", map[string]string{app.ProtectEnv: "0"}, nil), "WARN: control-plane protection DISABLED by TETHER_SANDBOX_PROTECT=0, so agents can write the catalog and run/", true},
		{"darwin", healthFor("darwin", nil, nil), "WARN: control-plane protection not applied on darwin", true},
		{"on but unusable", healthFor("linux", nil, fmt.Errorf("bwrap: setting up uid map: Permission denied")), "WARN: control-plane protection is on but unusable: bwrap: setting up uid map: Permission denied", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			logControlPlaneProtection(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, tc.health)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.want) || strings.HasPrefix(lines[0], "WARN") != tc.warn {
				t.Fatalf("logged %q; want one line starting %q", lines, tc.want)
			}
			if tc.name == "on but unusable" && !strings.Contains(lines[0], "except Codex") {
				t.Fatalf("unusable warning does not say Codex still runs: %q", lines[0])
			}
		})
	}
}

// mux doctor reports what the daemon says, since the daemon's environment
// decides protection. It warns when protection is off, and fails when it is on
// but bubblewrap cannot build a namespace, since every launch except Codex's
// is then refused. Without a daemon it says its answer is its own shell's.
func TestCheckSandboxProtect(t *testing.T) {
	for _, tc := range []struct {
		name       string
		health     app.ProtectionHealth
		fromDaemon bool
		want       string
		message    string
	}{
		{"on", healthFor("linux", nil, nil), true, statusOK, "on: agents cannot write the catalog or run/"},
		{"on, unusable", healthFor("linux", nil, fmt.Errorf("bwrap: No permissions to create a new namespace")), true, statusFail, "No permissions to create a new namespace"},
		{"switch off", healthFor("linux", map[string]string{app.ProtectEnv: "false"}, nil), true, statusWarn, "DISABLED by TETHER_SANDBOX_PROTECT=false"},
		{"darwin", healthFor("darwin", nil, nil), true, statusWarn, "CW-20261001-0138"},
		{"no daemon", healthFor("linux", nil, nil), false, statusOK, "this shell's environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkSandboxProtect(sandboxProtectHealth(tc.health), tc.fromDaemon)
			if r.Name != "sandbox-protect" || r.Status != tc.want || !strings.Contains(r.Message, tc.message) {
				t.Fatalf("check = %+v; want %s with a message containing %q", r, tc.want, tc.message)
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
	want := daemon.SandboxProtectHealth{Enabled: true, Reason: h.Reason, BwrapChecked: true, BwrapUsable: false, BwrapError: "denied"}
	if *h != want {
		t.Fatalf("health = %+v, want %+v", *h, want)
	}
}
