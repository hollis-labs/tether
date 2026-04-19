package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/chrispian/agent-mux/internal/launch"
)

type Session struct {
	ID         string
	Root       string
	LogPath    string
	PromptPath string
	PlanPath   string
}

// Create materializes a workspace for the session and writes the plan + prompt.
func Create(root, sessionID string, plan *launch.Plan) (*Session, error) {
	sessRoot := filepath.Join(root, sessionID)
	for _, sub := range Subdirs {
		if err := os.MkdirAll(filepath.Join(sessRoot, sub), 0o750); err != nil {
			return nil, err
		}
	}
	promptPath := filepath.Join(sessRoot, "prompts", "boot.md")
	if err := os.WriteFile(promptPath, []byte(plan.BootPrompt), 0o600); err != nil {
		return nil, err
	}
	planPath := filepath.Join(sessRoot, "state", "plan.json")
	pb, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(planPath, pb, 0o600); err != nil {
		return nil, err
	}
	logPath := filepath.Join(sessRoot, "logs", "session.log")
	return &Session{
		ID:         sessionID,
		Root:       sessRoot,
		LogPath:    logPath,
		PromptPath: promptPath,
		PlanPath:   planPath,
	}, nil
}

// Open reconstructs a Session value from a previously-created workspace
// without touching the filesystem. sessRoot is the absolute path already
// containing the session's subdirs (i.e., the value persisted in
// sessions.workspace). Used by the create-then-launch flow where workspace
// materialization happens at Create time and the Launch handler needs the
// derived paths without re-writing files.
func Open(sessRoot, sessionID string) *Session {
	return &Session{
		ID:         sessionID,
		Root:       sessRoot,
		LogPath:    filepath.Join(sessRoot, "logs", "session.log"),
		PromptPath: filepath.Join(sessRoot, "prompts", "boot.md"),
		PlanPath:   filepath.Join(sessRoot, "state", "plan.json"),
	}
}
