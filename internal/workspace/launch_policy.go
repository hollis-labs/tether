package workspace

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrWorktreeBranchInvalid refuses a branch before any worktree allocation.
// Its fixed message does not include the authored template or input values.
var ErrWorktreeBranchInvalid = errors.New("workspace: invalid worktree branch")

var branchInput = regexp.MustCompile(`\{\{\s*\.(ProjectID|SessionID|LogicalAgentID)\s*\}\}`)

// BranchInputs comes from the accepted launch plan and its canonical session.
// It cannot select a repository, filesystem root, credential or permission.
type BranchInputs struct {
	ProjectID      string
	SessionID      string
	LogicalAgentID string
}

// RenderWorktreeBranch supports only the named project/session/agent tokens.
// An empty authored name deliberately retains the detached-worktree policy;
// unknown or malformed templates refuse rather than silently becoming detached.
// Git validates the final branch ref before the caller allocates a directory.
func RenderWorktreeBranch(authored string, inputs BranchInputs) (string, error) {
	authored = strings.TrimSpace(authored)
	if authored == "" {
		return "", nil
	}
	if !utf8.ValidString(authored) || len(authored) > 512 {
		return "", ErrWorktreeBranchInvalid
	}
	values := map[string]string{"ProjectID": inputs.ProjectID, "SessionID": inputs.SessionID, "LogicalAgentID": inputs.LogicalAgentID}
	templated := strings.Contains(authored, "{{") || strings.Contains(authored, "}}")
	missing := false
	rendered := branchInput.ReplaceAllStringFunc(authored, func(token string) string {
		name := branchInput.FindStringSubmatch(token)[1]
		value := values[name]
		if value == "" {
			missing = true
		}
		return value
	})
	if missing || len(rendered) > 512 || !utf8.ValidString(rendered) || templated && strings.ContainsAny(rendered, "{}") || strings.Contains(rendered, "@{") || strings.ContainsRune(rendered, 0) {
		return "", ErrWorktreeBranchInvalid
	}
	// Fixed argv, no repository or shell. --branch is required: branch rules
	// additionally reject a leading dash and Git's ambiguous previous-checkout
	// shorthand is rejected above. No filesystem mutation is performed.
	if err := exec.Command("git", "check-ref-format", "--branch", rendered).Run(); err != nil { //nolint:gosec // Fixed non-mutating Git operation with a separately rendered ref.
		return "", ErrWorktreeBranchInvalid
	}
	return rendered, nil
}
