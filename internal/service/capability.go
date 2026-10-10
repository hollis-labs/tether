package service

import (
	"path/filepath"
	"runtime"
)

// LaunchUpdateCapability is startup metadata, not an authority grant. A caller
// marker alone cannot advertise service updates: require the installed unit,
// immutable runtime and this executable's selected provenance.
func LaunchUpdateCapability(root, executable string) string {
	if root == "" {
		return "foreground"
	}
	if runtime.GOOS != "linux" {
		return "none"
	}
	if !filepath.IsAbs(root) {
		return "none"
	}
	if _, err := verifiedUnit(root); err != nil {
		return "none"
	}
	r := Runtime{Root: root}
	version, err := r.Selector("current")
	if err != nil {
		return "none"
	}
	actual, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "none"
	}
	expected := filepath.Join(root, "versions", version, "tether")
	if actual != expected {
		return "none"
	}
	return "service"
}
