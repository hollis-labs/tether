package workerruntime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrResumeUnavailable is returned (and reported to the hub in launch_failed
// as "resume_unavailable: ...") when a launch asks to resume a provider
// session this worker can no longer continue: the provider conversation or
// the workspace it ran in is gone, or the request is not one this worker can
// honor. The worker never falls back to a fresh start; the hub decides.
var ErrResumeUnavailable = errors.New("resume_unavailable")

func resumeUnavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrResumeUnavailable, fmt.Sprintf(format, args...))
}

// providerSessionID matches the ids claude writes (UUIDs); anything else would
// be a path or an option and is refused before it reaches the filesystem or argv.
var providerSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// claudeConfigDir is where claude keeps its projects/ tree for this launch.
func (e *Executor) claudeConfigDir(env map[string]string) (string, error) {
	if e.ClaudeDir != "" {
		return e.ClaudeDir, nil
	}
	if d := env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d, nil
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", resumeUnavailable("cannot locate the claude config directory: %v", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// resolveResumeCwd finds the directory the provider session originally ran in.
// claude keys a conversation by its working directory, so resuming in any other
// directory would not find it. The directory is read from the session's own
// transcript and must still exist inside this worker's workspaces tree.
func (e *Executor) resolveResumeCwd(env map[string]string, id string) (string, error) {
	if !providerSessionID.MatchString(id) {
		return "", resumeUnavailable("invalid provider session id")
	}
	cfg, err := e.claudeConfigDir(env)
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(cfg, "projects", "*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", resumeUnavailable("provider session %s not found on this worker", id)
	}
	cwd, err := transcriptCwd(matches[0])
	if err != nil {
		return "", resumeUnavailable("provider session %s has no recorded workspace: %v", id, err)
	}
	wsRoot := filepath.Join(e.Root, "workspaces")
	if e.Config.WorkspaceRoot != "" {
		wsRoot = e.Config.WorkspaceRoot
	}
	rel, err := filepath.Rel(wsRoot, cwd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", resumeUnavailable("workspace of provider session %s is outside this worker's workspaces", id)
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", resumeUnavailable("workspace of provider session %s no longer exists", id)
	}
	return cwd, nil
}

// transcriptCwd returns the first "cwd" recorded in a claude transcript.
func transcriptCwd(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from a Glob under the claude config dir with a validated id
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for i := 0; i < 200 && sc.Scan(); i++ {
		var line struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Cwd != "" {
			return filepath.Clean(line.Cwd), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no cwd in transcript")
}

// withResume replaces any resume-like flags in the hub's args (separate-value and
// equals forms) with `--resume <id>`, so the hub's resolved args cannot start a
// different conversation than the one the hub approved.
func withResume(in []string, id string) []string {
	var out []string
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case "--resume", "-r", "--session-id":
			i++
		case "--continue", "-c", "--fork-session":
		default:
			// The equals forms carry their value in the same argument.
			if strings.HasPrefix(in[i], "--resume=") || strings.HasPrefix(in[i], "--session-id=") || strings.HasPrefix(in[i], "-r=") {
				continue
			}
			out = append(out, in[i])
		}
	}
	return append(out, "--resume", id)
}
