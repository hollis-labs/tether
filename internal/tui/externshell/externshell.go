// Package externshell opens a platform terminal window for two purposes:
//
//  1. AttachIn — spawns `mux sessions attach <id>` for attaching to a
//     running mux-managed session from a detail screen (F2 key).
//
//  2. BootWith — directly launches claude or opencode with the boot prompt
//     pre-loaded as the agent's system context. No mux session is created.
//     For opencode: uses a temp OPENCODE_CONFIG_DIR with an ephemeral agent
//     config so the prompt becomes the system prompt, not a user message.
//     For claude: pipes the prompt via stdin so claude reads it as the first
//     message then stays interactive.
//
// Terminal selection order (both functions):
//  1. MUX_TERMINAL env var template (e.g. `MUX_TERMINAL='iterm2 --detach -- %s'`)
//  2. macOS: iTerm2 if installed, else Terminal.app
//  3. Linux: $TERMINAL, then probes kitty/alacritty/gnome-terminal/xterm
package externshell

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return spawnTerminal(fmt.Sprintf("%s sessions attach %s", quote(mux), quote(sessionID)))
}

// BootWith opens a terminal with the given tool pre-loaded with the boot prompt.
// workDir is the project root to open the session in (~ already expanded by caller).
//
// opencode: creates a temp OPENCODE_CONFIG_DIR with an ephemeral agent config
// that sets the boot prompt as the agent system prompt, then launches
// `opencode --agent boot <workDir>` interactively.
//
// claude (and all other CLI tools): writes the boot prompt to a temp file and
// pipes it via stdin — claude reads it as the first message then stays interactive.
// The session opens in workDir.
func BootWith(bootPrompt, providerID, command, workDir string) error {
	switch providerID {
	case "opencode":
		return bootOpencode(bootPrompt, command, workDir)
	default:
		return bootClaude(bootPrompt, command, workDir)
	}
}

// agentName is the opencode agent key used in the ephemeral config. It must
// match an agent that opencode recognizes from any loaded config — using a
// globally-defined name ensures our override takes effect rather than falling
// back to the global default silently.
const agentName = "orchestrator"

// bootOpencode creates an ephemeral OPENCODE_CONFIG_DIR and a launcher script,
// then spawns opencode interactively in workDir.
//
// Why a script instead of an inline VAR=value command: on macOS, environment
// variables set inline in a command string don't reliably survive the
// osascript → iTerm2 → new-shell chain. Writing a shell script file and
// executing it sidesteps that entirely.
//
// Why "orchestrator" as the agent name: opencode falls back silently to the
// global default when --agent receives an unknown name. Using a name from the
// global config ("orchestrator") ensures our prompt override is applied.
func bootOpencode(bootPrompt, command, workDir string) error {
	tmpDir, err := os.MkdirTemp("", "mux-opencode-boot-*")
	if err != nil {
		return fmt.Errorf("create temp config dir: %w", err)
	}

	agentsDir := filepath.Join(tmpDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}

	// Boot prompt becomes the orchestrator's system prompt for this session.
	promptFile := filepath.Join(agentsDir, agentName+".md")
	if err := os.WriteFile(promptFile, []byte(bootPrompt), 0o600); err != nil {
		return fmt.Errorf("write boot prompt: %w", err)
	}

	// Minimal opencode config: override the orchestrator agent's system prompt.
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"agent": map[string]any{
			agentName: map[string]any{
				"prompt": "{file:./agents/" + agentName + ".md}",
			},
		},
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "opencode.json"), cfgBytes, 0o600); err != nil {
		return fmt.Errorf("write opencode config: %w", err)
	}

	// Shell script sets OPENCODE_CONFIG_DIR before exec'ing opencode.
	// Using a script file avoids env-var propagation issues through osascript.
	script := fmt.Sprintf("#!/bin/sh\nexport OPENCODE_CONFIG_DIR=%s\nexec %s --agent %s %s\n",
		quote(tmpDir), quote(command), agentName, quote(workDir))
	scriptPath := filepath.Join(tmpDir, "boot.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write boot script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil { //nolint:gosec // G302: shell script needs execute permission
		return fmt.Errorf("chmod boot script: %w", err)
	}

	return spawnTerminal(quote(scriptPath))
}

// bootClaude pipes the boot prompt via stdin. Claude reads it as the first
// message and stays interactive after responding.
func bootClaude(bootPrompt, command, workDir string) error {
	f, err := os.CreateTemp("", "mux-boot-*.md")
	if err != nil {
		return fmt.Errorf("write boot prompt: %w", err)
	}
	if _, err := f.WriteString(bootPrompt); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("write boot prompt: %w", err)
	}
	f.Close()

	shellCmd := fmt.Sprintf("cd %s && cat %s | %s --dangerously-skip-permissions",
		quote(workDir), quote(f.Name()), quote(command))
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
