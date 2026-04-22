// Package externshell opens a platform terminal window for two purposes:
//
//  1. AttachIn — spawns `mux sessions attach <id>` for attaching to a
//     running mux-managed session from a detail screen (F2 key).
//
//  2. BootWith — directly launches a tool (claude, opencode, …) with a
//     boot prompt pre-loaded. This is the boot-profile quicklaunch path:
//     no mux daemon session is created; the tool runs natively in the
//     terminal with the generated context as its first input.
//
// Terminal selection order (both functions):
//  1. MUX_TERMINAL env var template (e.g. `MUX_TERMINAL='iterm2 --detach -- %s'`)
//  2. macOS: iTerm2 if installed, else Terminal.app
//  3. Linux: $TERMINAL, then probes kitty/alacritty/gnome-terminal/xterm
package externshell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrUnsupportedPlatform is returned when no terminal can be spawned.
var ErrUnsupportedPlatform = errors.New("external terminal spawn not supported on this platform")

// AttachIn spawns a terminal running `mux sessions attach <sessionID>`.
// Used by the F2 escape hatch in attach/chat screens.
func AttachIn(sessionID string) error {
	mux, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate mux binary: %w", err)
	}
	cmd := fmt.Sprintf("%s sessions attach %s", quote(mux), quote(sessionID))
	return spawnTerminal(cmd)
}

// BootWith directly launches a tool with the boot prompt as initial input.
// This is the boot-profile quicklaunch path — no mux daemon session involved.
//
// How the boot prompt is delivered depends on the provider:
//   - "opencode": passed as a positional argument (`opencode run "<prompt>"`)
//   - all others (claude variants, etc.): piped via stdin with
//     `--dangerously-skip-permissions` so claude starts in auto-approve mode
//
// The prompt is written to a temp file to avoid shell argument-length limits
// and quoting issues. The temp file is left in /tmp for the OS to clean up.
func BootWith(bootPrompt, providerID, command string) error {
	// Write prompt to temp file — safer than inlining in shell args.
	f, err := os.CreateTemp("", "mux-boot-*.md")
	if err != nil {
		return fmt.Errorf("write boot prompt: %w", err)
	}
	if _, err := f.WriteString(bootPrompt); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("write boot prompt: %w", err)
	}
	f.Close()

	var shellCmd string
	switch providerID {
	case "opencode":
		// opencode run accepts the message as a positional arg.
		// The subshell $(cat file) expands inside the new terminal's shell.
		shellCmd = fmt.Sprintf("%s run \"$(cat %s)\"", quote(command), quote(f.Name()))
	default:
		// claude and other CLI tools: pipe prompt via stdin.
		// --dangerously-skip-permissions enables auto-approve mode.
		shellCmd = fmt.Sprintf("%s --dangerously-skip-permissions < %s", quote(command), quote(f.Name()))
	}

	return spawnTerminal(shellCmd)
}

// spawnTerminal dispatches to the right terminal emulator.
func spawnTerminal(cmd string) error {
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

// runTemplate fires the MUX_TERMINAL template, substituting %s with cmd.
func runTemplate(tmpl, cmd string) error {
	var rendered string
	if strings.Contains(tmpl, "%s") {
		rendered = fmt.Sprintf(tmpl, cmd)
	} else {
		rendered = tmpl + " " + cmd
	}
	return exec.Command("sh", "-c", rendered).Start() //nolint:gosec // G204: rendered is user-configured env template
}

// spawnMacTerminal prefers iTerm2 when installed; falls back to Terminal.app.
func spawnMacTerminal(cmd string) error {
	if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
		return spawnITerm2(cmd)
	}
	script := fmt.Sprintf(
		`tell application "Terminal" to activate
tell application "Terminal" to do script %q`, cmd)
	return exec.Command("osascript", "-e", script).Start() //nolint:gosec // G204: osascript input sanitized by %q
}

// spawnITerm2 opens a new iTerm2 tab running cmd.
func spawnITerm2(cmd string) error {
	script := fmt.Sprintf(`
tell application "iTerm2"
	activate
	tell current window
		create tab with default profile
		tell current session of current tab
			write text %q
		end tell
	end tell
end tell`, cmd)
	return exec.Command("osascript", "-e", script).Start() //nolint:gosec // G204: osascript input sanitized by %q
}

// spawnLinuxTerminal prefers $TERMINAL; probes common emulators on unset.
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
	return errors.New("no supported terminal found; set $MUX_TERMINAL or $TERMINAL")
}

// quote shell-escapes s with single quotes for `do script` / `sh -c` contexts.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
