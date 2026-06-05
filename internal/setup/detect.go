package setup

import (
	"os"

	gop "github.com/hollis-labs/go-providers/provider"
)

// DetectResult holds the outcome of one provider detection attempt.
type DetectResult struct {
	// Brand is the provider product name: "claude", "codex", or "opencode".
	Brand string
	// Found is true when the binary was located.
	Found bool
	// Path is the resolved absolute path when Found is true.
	Path string
	// Source describes how the binary was found: "env:CLAUDE_CLI_PATH",
	// "env:CODEX_CLI_PATH", "env:OPENCODE_CLI_PATH", "PATH", or "unset".
	Source string
}

// DetectProviders probes the three known CLI providers using the same
// Detect() plumbing as go-providers' adapters: $<BRAND>_CLI_PATH env var
// first, then exec.LookPath. Returns one DetectResult per brand in a fixed
// order: claude, codex, opencode.
func DetectProviders() []DetectResult {
	return []DetectResult{
		detect("claude", gop.NewClaudeAdapter(), "CLAUDE_CLI_PATH"),
		detect("codex", gop.NewCodexAdapter(), "CODEX_CLI_PATH"),
		detect("opencode", gop.NewOpencodeAdapter(), "OPENCODE_CLI_PATH"),
	}
}

// detect calls the adapter's Detect() and annotates the result with Source.
func detect(brand string, adapter gop.CLIAdapter, envVar string) DetectResult {
	path, ok := adapter.Detect()
	if !ok {
		return DetectResult{Brand: brand, Found: false, Source: "unset"}
	}
	source := "PATH"
	if os.Getenv(envVar) != "" {
		source = "env:" + envVar
	}
	return DetectResult{Brand: brand, Found: true, Path: path, Source: source}
}
