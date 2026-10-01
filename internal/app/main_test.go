package app

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the codex host-login lookup at an empty directory, so no
// test in this package links a launch to the developer's real ~/.codex
// (CW-20261001-0031). Tests that need a login set CODEX_HOME to a fixture.
//
// It also turns control-plane protection off by default, before any test
// runs, so a test that launches a stand-in CLI does not need bubblewrap
// (CW-20261001-0142). The tests of protection itself set
// Service.protectionStatus.
func TestMain(m *testing.M) {
	defaultProtectionStatus = func() ProtectionStatus {
		return ProtectionStatus{Reason: "off for internal/app unit tests"}
	}
	dir, err := os.MkdirTemp("", "tether-app-codex-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex home fixture:", err)
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
