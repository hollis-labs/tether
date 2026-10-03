package mcpadapter

import (
	"github.com/hollis-labs/tether/internal/store"
	"testing"
)

func TestSessionListRecoveryStates(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	for _, state := range []string{"detached", "orphaned"} {
		if err := f.db.CreateSession(store.SessionRow{ID: state, State: state}, nil); err != nil {
			t.Fatal(err)
		}
		// The daemon-only path also exercises the real HTTP list filter/DTO.
		result := callAnyTool(t, f.daemonOnly, "tether_session_list", map[string]any{"state": state})
		if result.IsError {
			t.Fatal(textOf(result))
		}
		data := parseToolJSON(t, result)
		sessions := data["sessions"].([]any)
		if len(sessions) != 1 || sessions[0].(map[string]any)["state"] != state {
			t.Fatalf("list %s: %+v", state, data)
		}
	}
}
