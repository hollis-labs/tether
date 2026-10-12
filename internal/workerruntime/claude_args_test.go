package workerruntime

import (
	"slices"
	"strings"
	"testing"
)

func has(args []string, flag, value string) bool {
	for i := range args {
		if args[i] == flag && (value == "" || (i+1 < len(args) && args[i+1] == value)) {
			return true
		}
	}
	return false
}

func TestClaudeArgsBypassReachesTheCommandLine(t *testing.T) {
	args := claudeArgs(nil, "bypass", "/w/.mcp.json")
	if !has(args, "--permission-mode", "bypassPermissions") {
		t.Fatalf("bypass must become --permission-mode bypassPermissions, got %v", args)
	}
	for _, want := range [][2]string{{"--print", ""}, {"--input-format", "stream-json"}, {"--output-format", "stream-json"}, {"--strict-mcp-config", ""}, {"--mcp-config", "/w/.mcp.json"}} {
		if !has(args, want[0], want[1]) {
			t.Fatalf("missing %v in %v", want, args)
		}
	}
}

func TestClaudeArgsDefaultAddsNoPermissionFlag(t *testing.T) {
	for _, mode := range []string{"", "default"} {
		args := claudeArgs(nil, mode, "/w/.mcp.json")
		if has(args, "--permission-mode", "") || has(args, "--dangerously-skip-permissions", "") {
			t.Fatalf("mode %q must not add a permission flag, got %v", mode, args)
		}
	}
}

func TestClaudeArgsReplacesHubSuppliedFlagsInsteadOfDuplicatingThem(t *testing.T) {
	in := []string{"--model", "opus", "--permission-mode", "plan", "--dangerously-skip-permissions", "--mcp-config", "/hub/path.json", "-p", "--verbose", "--add-dir", "/hub/dir"}
	args := claudeArgs(in, "bypass", "/w/.mcp.json")
	if n := strings.Count(strings.Join(args, " "), "--permission-mode"); n != 1 {
		t.Fatalf("want exactly one --permission-mode, got %d in %v", n, args)
	}
	if slices.Contains(args, "plan") || slices.Contains(args, "--dangerously-skip-permissions") || slices.Contains(args, "/hub/path.json") || slices.Contains(args, "/hub/dir") {
		t.Fatalf("hub-machine flags leaked into the worker command: %v", args)
	}
	if !has(args, "--model", "opus") {
		t.Fatalf("unrelated hub flags must be kept: %v", args)
	}
}
