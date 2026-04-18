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
		if err := os.MkdirAll(filepath.Join(sessRoot, sub), 0o755); err != nil {
			return nil, err
		}
	}
	promptPath := filepath.Join(sessRoot, "prompts", "boot.md")
	if err := os.WriteFile(promptPath, []byte(plan.BootPrompt), 0o644); err != nil {
		return nil, err
	}
	planPath := filepath.Join(sessRoot, "state", "plan.json")
	pb, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(planPath, pb, 0o644); err != nil {
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
