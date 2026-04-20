// Package externshell spawns a platform terminal window running
// `mux sessions attach <id>`. Used by the TUI's "open in external
// terminal" escape hatch when the in-TUI attach panel falls short —
// typically full-screen TUI providers (claude CLI, vim) where PTY
// size + ANSI fidelity matter more than tight integration.
//
// Honors the MUX_TERMINAL env var when set: its value is formatted
// with the full command as the single argument (e.g.
// `MUX_TERMINAL='kitty --detach -- %s'`). When unset, platform
// defaults apply:
//   - macOS: osascript driving Terminal.app's `do script`.
//   - Linux: $TERMINAL -e <cmd> (falls back to probing kitty /
//     alacritty / gnome-terminal / xterm if $TERMINAL is unset).
//   - anything else: returns ErrUnsupportedPlatform.
package externshell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrUnsupportedPlatform is returned by AttachIn when the host OS
// doesn't have a known external-terminal spawn path.
var ErrUnsupportedPlatform = errors.New("external terminal spawn not supported on this platform")

// AttachIn spawns a platform terminal running `mux sessions attach
// <sessionID>`. The current binary (resolved via os.Executable) is
// passed as an absolute path so the spawned terminal doesn't depend
// on PATH ordering.
//
// Returns nil on successful spawn (does not wait for the terminal to
// close). Errors surface platform issues or missing terminals.
func AttachIn(sessionID string) error {
	mux, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate mux binary: %w", err)
	}
	cmd := fmt.Sprintf("%s sessions attach %s", quote(mux), quote(sessionID))

	if tmpl := os.Getenv("MUX_TERMINAL"); tmpl != "" {
		return runTemplate(tmpl, cmd)
	}
	switch runtime.GOOS {
	case "darwin":
		return spawnMacTerminal(cmd)
	case "linux":
		return spawnLinuxTerminal(cmd)
	default:
		return ErrUnsupportedPlatform
	}
}

// runTemplate fires the MUX_TERMINAL template, substituting %s with
// cmd. If the template has no %s, cmd is appended on the end as a
// fallback for sloppy configurations.
func runTemplate(tmpl, cmd string) error {
	var rendered string
	if strings.Contains(tmpl, "%s") {
		rendered = fmt.Sprintf(tmpl, cmd)
	} else {
		rendered = tmpl + " " + cmd
	}
	return exec.Command("sh", "-c", rendered).Start() //nolint:gosec // G204: rendered is user-configured env template
}

// spawnMacTerminal uses osascript to tell Terminal.app to run a new
// tab with the mux-sessions-attach command. activates Terminal so
// the window comes to the front.
func spawnMacTerminal(cmd string) error {
	script := fmt.Sprintf(
		`tell application "Terminal" to activate
tell application "Terminal" to do script %q`, cmd)
	return exec.Command("osascript", "-e", script).Start() //nolint:gosec // G204: osascript input is sanitized by %q
}

// spawnLinuxTerminal prefers $TERMINAL; on unset, probes common
// emulators in PATH. All branches run the command as a detached
// child so the TUI keeps control of its own terminal.
func spawnLinuxTerminal(cmd string) error {
	if term := os.Getenv("TERMINAL"); term != "" {
		return exec.Command("sh", "-c", term+" -e "+cmd).Start() //nolint:gosec // G204: TERMINAL env is user-configured
	}
	candidates := []struct {
		bin  string
		args []string
	}{
		{"kitty", []string{"--detach"}},
		{"alacritty", []string{"-e"}},
		{"gnome-terminal", []string{"--"}},
		{"x-terminal-emulator", []string{"-e"}},
		{"xterm", []string{"-e"}},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err == nil {
			full := append([]string{}, c.args...)
			full = append(full, "sh", "-c", cmd)
			return exec.Command(c.bin, full...).Start() //nolint:gosec // G204: bin path is vetted via LookPath
		}
	}
	return errors.New("no supported terminal found on $PATH; set $MUX_TERMINAL or $TERMINAL")
}

// quote shell-escapes s with single quotes for safe embedding in
// `do script` / `sh -c` contexts. Handles embedded single quotes by
// breaking them out of the quoting.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
