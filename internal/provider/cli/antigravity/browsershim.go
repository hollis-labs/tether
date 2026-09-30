package antigravity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BrowserShimDirName is the directory, under a session workspace, that
// holds the browser shim.
const BrowserShimDirName = "browser-shim"

// browserShimScript is installed as every name in browserShimNames. It
// refuses without doing anything, so a browser open fails instead of
// putting a sign-in window on the user's screen.
const browserShimScript = `#!/bin/sh
echo "tether: browser open suppressed" >&2
exit 1
`

// browserShimNames are the openers agy's Go opener table looks up on PATH:
// open on macOS, xdg-open on Linux.
var browserShimNames = []string{"open", "xdg-open"}

// PlantBrowserShim writes the browser shim into dir and returns dir.
//
// SOFT GUARD, not a boundary. When agy's Keychain token is expired or
// revoked, print mode falls into its browser sign-in and runs `open <url>`,
// found on PATH. With dir first on PATH (PrependPATH) that lookup hits the
// shim, and agy waits out its 60s callback and fails with "authentication
// failed or timed out", which IsNotAuthenticated recognizes. It does not stop
// an absolute /usr/bin/open or a direct LaunchServices call; the hard
// boundary is a seatbelt deny rule that go-sandbox does not offer yet
// (CW-20260930-0221 R4). It also turns `open` into a failure for commands
// the agent runs, which a headless session has no use for.
func PlantBrowserShim(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("browser shim: %w", err)
	}
	for _, name := range browserShimNames {
		// 0o755: the shim must be executable to shadow the real opener.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(browserShimScript), 0o755); err != nil { //nolint:gosec // G306: an executable shim, not data
			return "", fmt.Errorf("browser shim: %w", err)
		}
	}
	return dir, nil
}

// PrependPATH returns env with dir first on PATH, keeping the rest of PATH
// as it was. Every PATH entry is rewritten, so whichever one the child ends
// up reading carries the shim. Without a PATH entry, PATH is set to dir.
func PrependPATH(env []string, dir string) []string {
	out := append([]string(nil), env...)
	found := false
	for i, kv := range out {
		rest, ok := strings.CutPrefix(kv, "PATH=")
		if !ok {
			continue
		}
		found = true
		if rest == "" {
			out[i] = "PATH=" + dir
		} else {
			out[i] = "PATH=" + dir + string(os.PathListSeparator) + rest
		}
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}
