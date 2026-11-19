package app

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/chatwire"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// The chat wire picks a backend from the session's provider and runtime kind (a worker session here: no local
// runtime to ask).
func TestChatRuntime(t *testing.T) {
	s := permissionWorkerHarness(t)
	set := func(brand, kind string) {
		t.Helper()
		raw, _ := json.Marshal(launch.Plan{ProviderBrand: brand, RuntimeKind: kind})
		if _, err := s.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id='s1'`, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		brand, kind string
		want        chatwire.Runtime
	}{
		{"claude", config.RuntimeKindStreamingStdio, chatwire.RuntimeClaudeStdio},
		{"claude-code", "stream-json", chatwire.RuntimeClaudeStdio},
		{"claude", config.RuntimeKindPTY, chatwire.RuntimeClaudePTY},
		{"codex", "app-server", chatwire.RuntimeCodexAppServer},
		{"codex", config.RuntimeKindSubprocess, ""},
		{"gemini", config.RuntimeKindPTY, ""},
		{"antigravity", config.RuntimeKindSubprocess, chatwire.RuntimeAgyStdio},
		{"antigravity", config.RuntimeKindPTY, ""},
		{"antigravity", config.RuntimeKindAPI, ""},
	}
	for _, c := range cases {
		set(c.brand, c.kind)
		got, err := s.ChatRuntime("s1")
		if c.want == "" {
			if !errors.Is(err, chatwire.ErrUnsupportedRuntime) {
				t.Errorf("%s/%s: ChatRuntime = %q, %v; want unsupported", c.brand, c.kind, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s/%s: ChatRuntime = %q, %v; want %q", c.brand, c.kind, got, err, c.want)
		}
	}
	if _, err := s.ChatBackend("s1", chatwire.RuntimeAgyStdio); !errors.Is(err, chatwire.ErrUnsupportedRuntime) {
		t.Errorf("worker agy backend: %v", err)
	}
	// A worker PTY session's transcript is on the worker: refused with the typed error.
	if _, err := s.ChatBackend("s1", chatwire.RuntimeClaudePTY); !errors.Is(err, chatwire.ErrUnsupportedRuntime) {
		t.Errorf("worker PTY backend: %v", err)
	}
	if _, err := s.ChatBackend("s1", chatwire.RuntimeCodexAppServer); !errors.Is(err, chatwire.ErrUnsupportedRuntime) {
		t.Errorf("codex backend: %v", err)
	}
}
