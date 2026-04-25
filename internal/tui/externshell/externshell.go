// Package externshell opens a platform terminal window for two purposes:
//
//  1. AttachIn — spawns `mux sessions attach <id>` for attaching to a
//     running mux-managed session from a detail screen (F2 key).
//
//  2. BootWith — directly launches claude-* or opencode with the boot prompt
//     pre-loaded as the agent's system context. No mux session is created.
//     For opencode: uses a temp OPENCODE_CONFIG_DIR with an ephemeral agent
//     config so the prompt becomes the system prompt, not a user message.
//     For claude-* variants: creates an ephemeral temp dir as the session root,
//     writes CLAUDE.md (boot prompt), .claude/settings.json (stub config), and
//     boot.sh that runs `cd <tmpDir> && <command> --add-dir <workDir>
//     --dangerously-skip-permissions`. The temp dir is removed when the session
//     exits via a trap in the launch script.
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
// args are passed to the provider binary between the command and our injected flags.
//
// Supported providerID values: "opencode", "claude", and any "claude-*" variant
// that is a direct-launch interactive CLI (e.g. "claude-code"). Streaming/
// programmatic variants like "claude-stream" are not supported and return an error.
//
// opencode: creates a temp OPENCODE_CONFIG_DIR with an ephemeral agent config
// that sets the boot prompt as the agent system prompt, then launches
// `opencode [args...] --agent boot <workDir>` interactively.
//
// claude-* variants: creates an ephemeral temp dir as the session root. The dir
// contains CLAUDE.md (boot prompt), .claude/settings.json (stub config), and
// boot.sh that runs `cd <tmpDir> && <command> [args...] --add-dir <workDir>
// --dangerously-skip-permissions`. The temp dir is removed on session exit via
// a trap. Claude auto-loads CLAUDE.md from the working directory; --add-dir
// gives file access to the real project root without auto-loading its CLAUDE.md.
func BootWith(bootPrompt, providerID, command string, args []string, workDir string) error {
	switch {
	case providerID == "opencode":
		return bootOpencode(bootPrompt, command, args, workDir)
	case strings.HasPrefix(providerID, "claude-") || providerID == "claude":
		return bootClaude(bootPrompt, command, args, workDir)
	case providerID == "codex-cli" || strings.HasPrefix(providerID, "codex"):
		return bootCodex(bootPrompt, command, args, workDir)
	default:
		return fmt.Errorf("unsupported provider for boot: %s", providerID)
	}
}

// bootOpencode creates an ephemeral OPENCODE_CONFIG_DIR and a launcher script,
// then spawns opencode interactively in workDir.
//
// Agent discovery in opencode comes from two sources:
//   - ~/.opencode/agents.json (global data dir, always loaded)
//   - agents.json in OPENCODE_CONFIG_DIR (if opencode reads it from there)
//
// We write agents.json to the temp config dir so opencode can discover our
// custom "mux-boot" agent without touching the user's global config.
// If opencode doesn't read agents.json from OPENCODE_CONFIG_DIR, it will
// fall back to the global default; the instructions_file path is absolute
// so the boot prompt is still loaded if the agent name matches.
//
// The script file avoids env-var propagation issues through osascript on macOS.
func bootOpencode(bootPrompt, command string, args []string, workDir string) error {
	const agent = "mux-boot"

	tmpDir, err := os.MkdirTemp("", "mux-opencode-boot-*")
	if err != nil {
		return fmt.Errorf("create temp config dir: %w", err)
	}

	agentsDir := filepath.Join(tmpDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}

	promptFile := filepath.Join(agentsDir, agent+".md")
	if err := os.WriteFile(promptFile, []byte(bootPrompt), 0o600); err != nil {
		return fmt.Errorf("write boot prompt: %w", err)
	}

	// agents.json — defines the custom agent so opencode can discover it.
	// Written to OPENCODE_CONFIG_DIR; opencode may read it from here alongside
	// (or instead of) the global ~/.opencode/agents.json.
	agentsDef, err := json.MarshalIndent(map[string]any{
		"agents": []map[string]any{
			{
				"name":              agent,
				"description":       "Agent Mux ephemeral boot session",
				"instructions_file": promptFile,
			},
		},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agents: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "agents.json"), agentsDef, 0o600); err != nil {
		return fmt.Errorf("write agents.json: %w", err)
	}

	// opencode.json — sets the prompt via the config "agent" block too,
	// covering whichever mechanism opencode actually uses.
	cfg, err := json.MarshalIndent(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"agent": map[string]any{
			agent: map[string]any{
				"prompt": "{file:./agents/" + agent + ".md}",
			},
		},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "opencode.json"), cfg, 0o600); err != nil {
		return fmt.Errorf("write opencode config: %w", err)
	}

	var argsStr strings.Builder
	for _, a := range args {
		argsStr.WriteString(" ")
		argsStr.WriteString(quote(a))
	}
	script := fmt.Sprintf("#!/bin/sh\ntrap 'rm -rf %s' EXIT\nexport OPENCODE_CONFIG_DIR=%s\n%s%s --agent %s %s\n",
		quote(tmpDir), quote(tmpDir), quote(command), argsStr.String(), agent, quote(workDir))
	scriptPath := filepath.Join(tmpDir, "boot.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write boot script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil { //nolint:gosec // G302: shell script needs execute permission
		return fmt.Errorf("chmod boot script: %w", err)
	}

	return spawnTerminal(quote(scriptPath))
}

// bootClaude creates an ephemeral temp dir as the agent's session root and
// launches claude from it. The temp dir contains:
//
//	CLAUDE.md              — boot prompt + project context instructions
//	.claude/settings.json  — ephemeral session config stub (mcpServers, approvedTools)
//	boot.sh                — launcher with trap-based cleanup on exit
//
// Claude auto-loads CLAUDE.md because it's the working directory. --add-dir
// gives file access to the real project root without auto-loading its CLAUDE.md.
// The temp dir is removed when the session exits (normal exit or interrupt).
func bootClaude(bootPrompt, command string, args []string, workDir string) error {
	tmpDir, err := os.MkdirTemp("", "mux-claude-boot-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}

	claudeMDPath := filepath.Join(tmpDir, "CLAUDE.md")
	claudeMD := bootPrompt + "\n\n---\n\nProject root: " + workDir +
		"\nTo load project context: Read " + workDir + "/CLAUDE.md" +
		"\nAfter context compaction: re-read this file (" + claudeMDPath + ")\n"
	if err := os.WriteFile(claudeMDPath, []byte(claudeMD), 0o600); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}

	settingsDir := filepath.Join(tmpDir, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	settings, err := json.MarshalIndent(map[string]any{
		"mcpServers":    map[string]any{},
		"approvedTools": []string{},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), settings, 0o600); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	var argsStr strings.Builder
	for _, a := range args {
		argsStr.WriteString(" ")
		argsStr.WriteString(quote(a))
	}
	// No exec — shell must survive so the EXIT trap fires and removes tmpDir.
	script := fmt.Sprintf("#!/bin/sh\ntrap 'rm -rf %s' EXIT\ncd %s\n%s%s --add-dir %s --dangerously-skip-permissions\n",
		quote(tmpDir), quote(tmpDir), quote(command), argsStr.String(), quote(workDir))
	scriptPath := filepath.Join(tmpDir, "boot.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write boot script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil { //nolint:gosec // G302: shell script needs execute permission
		return fmt.Errorf("chmod boot script: %w", err)
	}

	return spawnTerminal(quote(scriptPath))
}

// bootCodex creates an ephemeral AGENTS.md in workDir and launches the Codex
// interactive REPL from that directory. Codex auto-loads AGENTS.md from the
// working directory as its system context.
//
// AGENTS.md is only written when one does not already exist; if we created it,
// a trap in the launch script removes it when the terminal session ends so the
// project root is left clean.
func bootCodex(bootPrompt, command string, args []string, workDir string) error {
	tmpDir, err := os.MkdirTemp("", "mux-codex-boot-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}

	agentsPath := filepath.Join(workDir, "AGENTS.md")
	var cleanupLine string
	if _, serr := os.Stat(agentsPath); os.IsNotExist(serr) {
		if werr := os.WriteFile(agentsPath, []byte(bootPrompt), 0o600); werr != nil {
			return fmt.Errorf("write AGENTS.md: %w", werr)
		}
		cleanupLine = fmt.Sprintf("trap 'rm -f %s' EXIT\n", quote(agentsPath))
	}

	var argsStr strings.Builder
	for _, a := range args {
		argsStr.WriteString(" ")
		argsStr.WriteString(quote(a))
	}
	// No exec — shell must survive so the EXIT trap fires and removes AGENTS.md.
	script := fmt.Sprintf("#!/bin/sh\n%scd %s\n%s%s\n",
		cleanupLine, quote(workDir), quote(command), argsStr.String())
	scriptPath := filepath.Join(tmpDir, "boot.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write boot script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil { //nolint:gosec // G302: shell script needs execute permission
		return fmt.Errorf("chmod boot script: %w", err)
	}

	return spawnTerminal(quote(scriptPath))
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
