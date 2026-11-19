package chatwire

import (
	"context"
	"testing"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/agent/chat/chattest"
)

// The chathost-backed Control against the wire's lease rules. The Control is not a chat.Bumper: a generation bump is
// Tether's, so the conformance bump writes a resume session (which moves the conversation in the session transaction)
// and then runs the post-commit sync the relaunch path runs.
func TestControlConformance(t *testing.T) {
	envs := map[chat.Control]*env{}
	chattest.ControlConformance(t, func(t *testing.T) chat.Control {
		e := newEnv(t)
		e.session(t, "sa", "agent-a", "")
		e.session(t, "sb", "agent-b", "")
		envs[e.ctl] = e
		return e.ctl
	}, chattest.WithBump(func(t *testing.T, c chat.Control, conversationID string) {
		t.Helper()
		ctx := context.Background()
		e := envs[c]
		tether, err := e.ctl.TetherConversation(ctx, conversationID)
		if err != nil {
			t.Fatal(err)
		}
		target, err := e.resolver.ByConversation(ctx, tether)
		if err != nil {
			t.Fatal(err)
		}
		e.relaunch(t, target.Seat)
		if got, err := e.ctl.Sync(ctx, tether); err != nil || got != conversationID {
			t.Fatalf("Sync = %q, %v", got, err)
		}
	}))
}
