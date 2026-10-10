package workspace

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderedWorktreeBranchUsesOnlyAcceptedInputs(t *testing.T) {
	input := BranchInputs{ProjectID: "project", SessionID: "canonical-session", LogicalAgentID: "worker"}
	for _, item := range []struct{ authored, want string }{
		{"", ""}, {"task/literal-branch", "task/literal-branch"},
		{"tether/{{.ProjectID}}/{{.SessionID}}", "tether/project/canonical-session"},
		{"task/{{ .LogicalAgentID }}/{{ .SessionID }}", "task/worker/canonical-session"},
	} {
		got, err := RenderWorktreeBranch(item.authored, input)
		if err != nil || got != item.want {
			t.Fatal(item.authored, got, err)
		}
		again, err := RenderWorktreeBranch(item.authored, input)
		if err != nil || again != got {
			t.Fatal("accepted retry changed branch identity", again, err)
		}
	}
}

func TestRenderedWorktreeBranchRefusesUnknownMissingAndMalformedInputs(t *testing.T) {
	input := BranchInputs{ProjectID: "project", SessionID: "session"}
	for _, authored := range []string{
		"task/{{.Unknown}}", "task/{{.SessionID}", "task/{{printf .SessionID}}", "task/{{.LogicalAgentID}}",
		"task/{{.ProjectID}}{{", "task/{{.SessionID}}}", "task/with space", "-option", "@{-1}",
		"task/../escape", "task/name.lock", "task/trailing.", "task/\x00null", strings.Repeat("x", 513),
	} {
		got, err := RenderWorktreeBranch(authored, input)
		if !errors.Is(err, ErrWorktreeBranchInvalid) || got != "" {
			t.Fatal(authored, got, err)
		}
	}
	input.SessionID = "bad..ref"
	if _, err := RenderWorktreeBranch("task/{{.SessionID}}", input); !errors.Is(err, ErrWorktreeBranchInvalid) {
		t.Fatal("invalid rendered ref accepted", err)
	}
}
