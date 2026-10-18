package workerruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/hollis-labs/tether/internal/workerwire"
)

// Claude lifecycle hooks the worker plants for a session whose launch carries AgentEvents (CW-20261014-0143, slice 3b-1).
//
// The settings file is passed with `claude --settings <file>`, which Claude loads as an additional layer: its hooks run in
// addition to the user's and the project's, they never replace them. The command is this worker's own executable, by absolute
// path, with only the session id and the spool directory on its command line (no token, nothing secret). It records
// content-free activity events and never decides anything (`tether-worker hook claude`, see agentevents.RecordClaudeHook).
// Planting is best effort and fails open: any refusal below launches the session without hooks, never fails the launch.

// claudeHookEvents are the events agentevents.ClaudeHook maps.
var claudeHookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest",
	"PermissionDenied", "Notification", "SubagentStart", "SubagentStop", "Stop", "SessionEnd",
}

// hookTimeoutSeconds bounds one hook run on Claude's side, on top of the recorder's own 200 ms budget, so a hung disk can never
// stall the agent for more than this.
const hookTimeoutSeconds = 5

var hookSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// safeShellWord reports whether s can sit inside single quotes in a shell command line unchanged.
func safeShellWord(s string) bool {
	return s != "" && !strings.ContainsAny(s, "'\x00\n\r")
}

// checkHookBinary refuses an executable other users could replace: it must be an absolute path to a regular file that is not
// group- or other-writable, in a directory that is not group- or other-writable, and not a symlink the hook could be redirected
// through later.
func checkHookBinary(path string) error {
	if !filepath.IsAbs(path) || !safeShellWord(path) {
		return errors.New("hook binary path is not an absolute, shell-safe path")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("hook binary is not a regular file")
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("hook binary is group- or other-writable")
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if di.Mode().Perm()&0o022 != 0 && di.Mode()&os.ModeSticky == 0 {
		return errors.New("hook binary directory is group- or other-writable")
	}
	return nil
}

// agentHooksSettings is the --settings JSON: one command hook per event, matcher-less, observe-only.
func agentHooksSettings(binary, session, spool string) ([]byte, error) {
	if !hookSessionID.MatchString(session) {
		return nil, errors.New("session id is not a safe hook argument")
	}
	if !filepath.IsAbs(spool) || !safeShellWord(spool) {
		return nil, errors.New("spool directory is not an absolute, shell-safe path")
	}
	command := fmt.Sprintf("'%s' hook claude --session '%s' --spool '%s'", binary, session, spool)
	hooks := map[string]any{}
	for _, event := range claudeHookEvents {
		hooks[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": hookTimeoutSeconds}}}}
	}
	return json.MarshalIndent(map[string]any{"hooks": hooks}, "", " ")
}

// plantAgentHooks prepares the session's private spool directory and writes the hooks settings file under the session workspace
// root (outside the git workroot), and returns the settings path to pass to claude. Any error means: launch without hooks.
//
// The agent can write its workspace, so nothing here follows a link or trusts what it finds: the events directory is created
// with Mkdir (not MkdirAll) and, if it already exists (a resume), must be a real directory, not a symlink, owned by this user,
// mode 0700; and the settings file is written fresh at every launch (temp file, then rename over any previous one) and never
// reused from disk.
func (e *Executor) plantAgentHooks(wsRoot, session string) (settings string, err error) {
	if err := checkHookBinary(e.Binary); err != nil {
		return "", err
	}
	spool := filepath.Join(wsRoot, "events")
	if err := ensurePrivateDir(spool); err != nil {
		return "", err
	}
	raw, err := agentHooksSettings(e.Binary, session, spool)
	if err != nil {
		return "", err
	}
	settings = filepath.Join(wsRoot, "agent-hooks.json")
	if err := writeFresh(settings, raw); err != nil {
		return "", err
	}
	return settings, nil
}

// ensurePrivateDir creates dir (0700) or accepts an existing one only if it is a real directory owned by this user with mode
// exactly 0700. Lstat, so a symlink in its place is refused rather than followed.
func ensurePrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs the search bit; Mkdir honors the umask
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("events path is not a real directory")
	}
	if fi.Mode().Perm() != 0o700 {
		return errors.New("events directory is not mode 0700")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() { //nolint:gosec // G115: uid fits int
		return errors.New("events directory is not owned by this user")
	}
	return nil
}

// writeFresh writes path with content 0600 through a new temp file in the same directory and a rename, so a symlink or file the
// agent left at path is replaced, never written through or reused.
func writeFresh(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-hooks-*") // O_EXCL, 0600
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(content)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// agentHookArgs are the extra claude arguments for a launch with AgentEvents set, or none. Observability only: a refusal
// launches the session without hooks and logs why; it never fails the launch.
func (e *Executor) agentHookArgs(wsRoot string, spec workerwire.LaunchSpec) []string {
	if !spec.AgentEvents {
		return nil
	}
	settings, err := e.plantAgentHooks(wsRoot, spec.Session)
	if err != nil {
		// Visible, not silent: a log line and a marker file in the session workspace (agent_events=disabled:<reason>).
		reason := err.Error()
		log.Printf("worker: agent events disabled for session %s: %s", spec.Session, reason)
		marker := "agent_events=disabled:" + strings.ReplaceAll(reason, "\n", " ") + "\n"
		_ = writeFresh(filepath.Join(wsRoot, "agent-events.disabled"), []byte(marker))
		return nil
	}
	_ = os.Remove(filepath.Join(wsRoot, "agent-events.disabled")) // a resume that works clears an earlier refusal
	return []string{"--settings", settings}
}
