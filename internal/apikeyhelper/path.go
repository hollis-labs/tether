package apikeyhelper

import (
	"os"
	"os/exec"
	"path/filepath"
)

// ResolvePath returns an absolute path to the mux-apikey-helper binary.
// Resolution order: $MUX_APIKEY_HELPER env override, then a sibling next to
// the mux binary, then $PATH lookup. Returns empty when not found.
func ResolvePath() string {
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

// ResolveNamedPath resolves an operator-supplied helper binary name or path.
// Absolute and relative paths are validated directly; bare names are looked up
// on $PATH.
func ResolveNamedPath(name string) string {
	if name == "" {
		return ""
	}
	if filepath.Base(name) != name || filepath.IsAbs(name) {
		if abs, err := filepath.Abs(name); err == nil {
			name = abs
		}
		if eval, err := filepath.EvalSymlinks(name); err == nil {
			name = eval
		}
		if isExecutableFile(name) {
			return name
		}
		return ""
	}
	if path, err := exec.LookPath(name); err == nil && isExecutableFile(path) {
		return path
	}
	return ""
}

// isExecutableFile reports whether path is a regular file with any execute
// bit set. Used by helper resolution to skip non-executable matches.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // operator-controlled helper paths are the intended trust boundary here
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
