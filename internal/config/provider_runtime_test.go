package config

import (
	"testing"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
)

// Catalog runtime-kind tokens stay Tether's own; RuntimeMode is where they
// become the leaf modes the libs take (agentkit v0.12.0 rejects the old
// spellings). A catalog or a persisted plan written with the old tokens must
// keep resolving.
func TestRuntimeMode(t *testing.T) {
	for _, tc := range []struct {
		token string
		mode  runtimes.Mode
		debug bool
	}{
		{"subprocess", runtimes.ModeSubprocessPerTurn, false},
		{"exec", runtimes.ModeSubprocessPerTurn, false},
		{"subprocess-per-turn", runtimes.ModeSubprocessPerTurn, false},
		{"serve-http", runtimes.ModeHTTPSSE, false},
		{"http-sse", runtimes.ModeHTTPSSE, false},
		{"pty-debug", runtimes.ModePTY, true},
		{"pty", runtimes.ModePTY, false},
		{"app-server", runtimes.ModeJSONRPCStdio, false},
		{"jsonrpc-stdio", runtimes.ModeJSONRPCStdio, false},
		{"streaming-stdio", runtimes.ModeStreamingStdio, false},
		{"claude-code", runtimes.ModeStreamingStdio, false},
		{"Streaming_Stdio", runtimes.ModeStreamingStdio, false},
	} {
		mode, debug, ok := RuntimeMode(tc.token)
		if !ok || mode != tc.mode || debug != tc.debug {
			t.Errorf("RuntimeMode(%q) = %q, debug=%v, ok=%v; want %q, debug=%v", tc.token, mode, debug, ok, tc.mode, tc.debug)
		}
		if !mode.Valid() {
			t.Errorf("RuntimeMode(%q) = %q is not a valid leaf mode", tc.token, mode)
		}
	}
	for _, token := range []string{"api", "", "agents_md", "warp-drive"} {
		if mode, _, ok := RuntimeMode(token); ok {
			t.Errorf("RuntimeMode(%q) = %q, want no mode", token, mode)
		}
	}
}

func TestCatalogFlags(t *testing.T) {
	for _, tc := range []struct {
		brand      string
		args, want []string
	}{
		{"opencode", []string{"run"}, []string{}},
		{"opencode", []string{"run", "--print-logs"}, []string{"--print-logs"}},
		{"opencode", []string{"--print-logs", "run"}, []string{"--print-logs", "run"}},
		{"claude", []string{"run"}, []string{"run"}},
		{"codex", nil, []string{}},
	} {
		got := CatalogFlags(tc.brand, tc.args)
		if len(got) != len(tc.want) || (len(got) > 0 && !equalStrings(got, tc.want)) {
			t.Errorf("CatalogFlags(%q, %q) = %q, want %q", tc.brand, tc.args, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
