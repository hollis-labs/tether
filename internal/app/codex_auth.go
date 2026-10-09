package app

import (
	"os"

	"github.com/hollis-labs/tether/internal/launchartifacts"
)

// captureCodexHome snapshots the explicitly configured daemon provider home
// before Compile/Prepare redirects CODEX_HOME into a fresh boot directory.
// DEC-036 authorizes only its existing auth.json; HOME is never a fallback.
func captureCodexHome() (*launchartifacts.CodexHome, error) {
	return launchartifacts.CaptureCodexHome(os.Getenv("CODEX_HOME"))
}
