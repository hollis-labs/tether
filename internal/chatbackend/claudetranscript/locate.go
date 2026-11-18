package claudetranscript

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ProjectsDir is where Claude Code keeps transcripts: $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects.
func ProjectsDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// ErrNotFound is Locate's answer when no project directory holds the session's transcript (yet: Claude Code creates the
// file with its first batch of lines, after the first prompt).
var ErrNotFound = errors.New("claudetranscript: transcript not found")

// Locate finds the transcript of a Claude session id under projectsDir. Claude names the project directory after the
// session's working directory with every non-alphanumeric character replaced by '-'; Locate does not rebuild that name,
// it looks in every project directory, so a cwd it would encode differently still resolves.
func Locate(projectsDir, sessionID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", errors.New("claudetranscript: session id is not a safe file name")
	}
	matches, err := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", ErrNotFound
	}
	// Two project directories with the same session file would be a copied tree; the newest is the live one.
	best, bestMod := "", int64(-1)
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().UnixNano() > bestMod {
			best, bestMod = m, fi.ModTime().UnixNano()
		}
	}
	if best == "" {
		return "", ErrNotFound
	}
	return best, nil
}
