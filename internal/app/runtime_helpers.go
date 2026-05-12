package app

import (
	"os"
	"os/exec"
	"path/filepath"

	gop "github.com/hollis-labs/go-providers/provider"
)

// providerHasSessionIDContinuity reports whether the given provider ID
// participates in the CLI session-ID continuity mechanism (preset +
// OnSessionID callback). Adapters in this set use --resume or --session
// flags to maintain conversation continuity across daemon restarts.
func providerHasSessionIDContinuity(providerID string) bool {
	switch providerID {
	case "claude-code", "claude-stream", "opencode":
		return true
	}
	return false
}

// goproviderCLIAdapter maps a catalog adapter name to the corresponding
// go-providers CLIAdapter. Returns nil for unknown names; callers skip
// registration silently (a validation error catches unknown names earlier).
func goproviderCLIAdapter(name string) gop.CLIAdapter {
	switch name {
	case "claude":
		return gop.NewClaudeAdapter()
	case "codex":
		return gop.NewCodexAdapter()
	default:
		return nil
	}
}

// resolveAPIKeyHelperPath returns an absolute path to the mux-apikey-helper
// binary. Resolution order: $MUX_APIKEY_HELPER env override, then a sibling
// next to the mux binary, then $PATH lookup. Returns empty when not found.
func resolveAPIKeyHelperPath() string {
	if override := os.Getenv("MUX_APIKEY_HELPER"); override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			override = abs
		}
		if eval, err := filepath.EvalSymlinks(override); err == nil {
			override = eval
		}
		if isExecutableFile(override) {
			return override
		}
	}
	if exe, err := os.Executable(); err == nil {
		if eval, eerr := filepath.EvalSymlinks(exe); eerr == nil {
			exe = eval
		}
		candidate := filepath.Join(filepath.Dir(exe), "mux-apikey-helper")
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath("mux-apikey-helper"); err == nil && isExecutableFile(path) {
		return path
	}
	return ""
}

// isExecutableFile reports whether path is a regular file with any execute
// bit set. Used by API key helper resolution to skip non-executable matches.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // G703: operator-controlled override/path lookup is the intended trust boundary here
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
