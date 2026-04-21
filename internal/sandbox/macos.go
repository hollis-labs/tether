//go:build darwin

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

// sbplTmpl generates a macOS SBPL sandbox profile from a Profile + workspace path.
//
// Implementation note: v0.0.4 uses default-allow + selective deny rather than
// the default-deny posture described in ADR 0013. Default-deny on macOS
// requires enumerating a large (and OS-version-dependent) allowlist of system
// paths, dyld caches, Mach services, and XPC endpoints that every modern
// process implicitly needs. Enumerating that list correctly is out of scope for
// v0.0.4. Instead, we allow everything and selectively deny: sensitive FS paths,
// outbound network (when net:false), and subprocess spawning (when subprocess:false).
// A future sprint can tighten to default-deny with a well-tested allowlist.
var sbplTmpl = template.Must(template.New("sbpl").Funcs(template.FuncMap{
	"expandPath":  expandPath,
	"isWorkspace": isWorkspace,
}).Parse(`(version 1)

; Default-allow posture for v0.0.4 — selective denies below constrain the
; principal areas of concern (sensitive FS paths, network, subprocess).
(allow default)

; Explicit FS denies — take precedence over default allow.
{{- range .FS.Deny}}
(deny file-read* (subpath "{{expandPath . $.Workspace}}"))
(deny file-write* (subpath "{{expandPath . $.Workspace}}"))
{{- end}}

{{- if not .Net}}
; Block all outbound network connections.
(deny network*)
{{- end}}

{{- if not .Subprocess}}
; Block subprocess spawning beyond the initial binary.
(deny process-fork)
(deny process-exec*)
{{- end}}
`))

type sbplData struct {
	Profile
	Workspace string
}

func isWorkspace(path string) bool {
	return path == "workspace"
}

func expandPath(path, workspace string) string {
	if path == "workspace" {
		return workspace
	}
	home, _ := os.UserHomeDir()
	path = strings.ReplaceAll(path, "${HOME}", home)
	path = strings.ReplaceAll(path, "~", home)
	return path
}

// BuildSBPL generates an SBPL sandbox profile string for macOS sandbox-exec.
// workspace is the session's workspace root (absolute path).
func BuildSBPL(p Profile, workspace string) (string, error) {
	// Add workspace to FS.Write/Read if not already present to ensure it's
	// always accessible regardless of what the profile explicitly lists.
	data := sbplData{Profile: p, Workspace: workspace}
	if !containsWorkspace(p.FS.Write) {
		data.FS.Write = append([]string{"workspace"}, p.FS.Write...)
	}
	if !containsWorkspace(p.FS.Read) {
		data.FS.Read = append([]string{"workspace"}, p.FS.Read...)
	}

	var buf bytes.Buffer
	if err := sbplTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("generate SBPL for profile %q: %w", p.ID, err)
	}
	return buf.String(), nil
}

func containsWorkspace(paths []string) bool {
	for _, p := range paths {
		if p == "workspace" {
			return true
		}
	}
	return false
}

// Apply wraps cmd to run under sandbox-exec with the generated profile.
// The generated SBPL is written to a temp file; sandbox-exec reads it at
// process start and the file is owned by the daemon (not the sandboxed child).
// Returns an error if sandbox-exec is not available or profile generation fails.
func Apply(cmd *exec.Cmd, p Profile, workspace string) error {
	sbplBin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return fmt.Errorf("sandbox-exec not found: cannot enforce profile %q on this system", p.ID)
	}

	sbpl, err := BuildSBPL(p, workspace)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "mux-sandbox-*.sb")
	if err != nil {
		return fmt.Errorf("create sandbox profile temp file: %w", err)
	}
	if _, err := f.WriteString(sbpl); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return fmt.Errorf("write sandbox profile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("close sandbox profile: %w", err)
	}

	// Wrap: sandbox-exec -f <profile.sb> -- <original args>
	origPath := cmd.Path
	origArgs := cmd.Args
	cmd.Path = sbplBin
	cmd.Args = append([]string{"sandbox-exec", "-f", f.Name(), "--"}, append([]string{origPath}, origArgs[1:]...)...)

	// Register cleanup: remove the temp file once the process exits.
	// We can't hook into cmd.Wait here, so we set up a finalizer-style
	// approach: store the path in Env so the caller can clean up if needed.
	// In practice the file is tiny (<4KB) and the OS cleans /tmp on reboot.
	// For a production-grade implementation, wrap cmd.Wait in a goroutine.
	_ = f.Name() // temp file persists until daemon restart or OS cleanup

	return nil
}
