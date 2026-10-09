package setup

import (
	"os"

	"github.com/hollis-labs/substrate/harness/adapters/registry"
)

// DetectResult holds the outcome of one provider detection attempt.
type DetectResult struct {
	// Brand is the runtime's registry id: "claude", "codex", "opencode",
	// "copilot", "pi", "antigravity", ...
	Brand string
	// Found is true when the binary was located.
	Found bool
	// Path is the resolved absolute path when Found is true.
	Path string
	// Source describes how the binary was found: "env:<VAR>" for the
	// runtime's env override (CLAUDE_CLI_PATH, COPILOT_CLI_PATH, ...),
	// "PATH" otherwise, or "unset".
	Source string
}

// DetectProviders probes every runtime in the go-providers registry the way
// its launch does: the descriptor's env override first, then PATH and the
// install directories (registry.Descriptor.LookPath). One result per
// runtime, in the registry's order, so a runtime added to the registry is
// detected with no Tether change (CW-20260930-0106).
func DetectProviders() []DetectResult {
	all := registry.All()
	out := make([]DetectResult, 0, len(all))
	for _, d := range all {
		out = append(out, detect(d))
	}
	return out
}

func detect(d registry.Descriptor) DetectResult {
	brand := string(d.ID)
	path, err := d.LookPath()
	if err != nil || path == "" {
		return DetectResult{Brand: brand, Found: false, Source: "unset"}
	}
	source := "PATH"
	if d.EnvOverride != "" && os.Getenv(d.EnvOverride) != "" {
		source = "env:" + d.EnvOverride
	}
	return DetectResult{Brand: brand, Found: true, Path: path, Source: source}
}
