package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain removes the launching Codex runtime's CODEX_* environment before
// tests run. Individual tests can still set their own variables and fixtures.
// It points the explicit Codex provider home at an owned stand-in auth source,
// so no test links a launch to real credentials. DEC-036 requires an existing
// source; individual absence/refusal tests set their own CODEX_HOME.
//
// It also turns control-plane protection off by default, before any test
// runs, so a test that launches a stand-in CLI does not need bubblewrap
// (CW-20261001-0142). The tests of protection itself set
// Service.protectionStatus.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CODEX_") {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(os.Stderr, "clear inherited codex environment:", err)
				os.Exit(1)
			}
		}
	}
	defaultProtectionStatus = func() ProtectionStatus {
		return ProtectionStatus{Reason: "off for internal/app unit tests"}
	}
	dir, err := os.MkdirTemp("", "tether-app-codex-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex home fixture:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(fixtureAuth), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "codex auth fixture:", err)
		os.Exit(1)
	}
	if err := os.Setenv("CODEX_HOME", dir); err != nil {
		fmt.Fprintln(os.Stderr, "codex home fixture:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
