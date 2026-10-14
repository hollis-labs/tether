package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil"
)

// TestMain removes the launching Codex runtime's CODEX_* environment before
// tests run. Individual tests can still set their own variables and fixtures.
// It points the explicit Codex provider home at an owned stand-in auth source,
// so no test links a launch to real credentials. DEC-036 requires an existing
// source; individual absence/refusal tests set their own CODEX_HOME.
//
// It also turns control-plane protection off by default, before any test
// runs, so a test that launches a stand-in CLI does not need bubblewrap
// (CW-20261001-0142). TMPDIR is canonicalized first (macOS /var symlink). The tests of protection itself set
// Service.protectionStatus.
func TestMain(m *testing.M) {
	if err := testutil.CanonicalizeTempEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "canonical temp dir:", err)
		os.Exit(1)
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CODEX_") {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(os.Stderr, "clear inherited codex environment:", err)
				os.Exit(1)
			}
		}
	}
	// Open starts new databases from one migrated image: replaying every
	// migration costs ~1.8s per Open under -race and these tests open hundreds.
	store.UseMigratedTemplate()
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
