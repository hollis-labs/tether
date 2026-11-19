package chatwire

import (
	"context"
	"testing"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/agent/chat/chattest"
)

// oneConversation maps every chat conversation to one Tether conversation, as the conformance suite names its own.
type oneConversation struct{ tether string }

func (m oneConversation) TetherConversation(context.Context, string) (string, error) {
	return m.tether, nil
}
func (m oneConversation) CurrentFor(context.Context, string) (string, error) {
	return "conv_conformance", nil
}

func newTestRunner(t *testing.T, rt Runtime) (*Runner, *env, *fakeSessions) {
	t.Helper()
	e := newEnv(t)
	e.session(t, "s1", "seat", "")
	target, err := e.resolver.BySeat(context.Background(), "seat")
	if err != nil {
		t.Fatal(err)
	}
	sessions := newFakeSessions(t)
	sessions.set("s1", rt)
	r, err := NewRunner(RunnerConfig{Mapping: oneConversation{tether: target.ConversationID}, Target: e.resolver.ByConversation, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, e, sessions
}

// The runner over each Claude backend against the runner rules of the wire.
func TestRunnerConformance(t *testing.T) {
	for _, rt := range []Runtime{RuntimeClaudePTY, RuntimeClaudeStdio, RuntimeCodexAppServer} {
		t.Run(string(rt), func(t *testing.T) {
			chattest.RunnerConformance(t, chattest.RunnerHarness{
				New: func(t *testing.T) chat.Runner {
					r, _, _ := newTestRunner(t, rt)
					return r
				},
				Text:     "hello",
				LongText: longText,
				// No ApprovalText: Tether runs keep the agent's own permission handling (Info.Approvals false).
			})
		})
	}
}

func TestRunnerInfo(t *testing.T) {
	for _, rt := range []Runtime{RuntimeClaudePTY, RuntimeClaudeStdio, RuntimeCodexAppServer} {
		r, e, _ := newTestRunner(t, rt)
		info, err := r.Info(context.Background(), "conv_conformance")
		target, _ := e.resolver.BySeat(context.Background(), "seat")
		if err != nil || info.Deltas != rt.Deltas() || info.Approvals || info.RunnerRef != "s1/"+target.AttemptID {
			t.Errorf("%s: Info = %+v, %v", rt, info, err)
		}
	}
}

// A relaunch moves the runner to the new session's backend and signals Replaced.
func TestRunnerMovedRebindsAndSignals(t *testing.T) {
	r, e, sessions := newTestRunner(t, RuntimeClaudeStdio)
	ctx := context.Background()
	if _, err := r.Info(ctx, "conv_conformance"); err != nil {
		t.Fatal(err)
	}
	next := e.relaunch(t, "seat")
	sessions.set(next, RuntimeClaudeStdio)
	r.Moved(ctx, "conv_conformance")
	if got := <-r.Replaced(); got != "conv_conformance" {
		t.Fatalf("Replaced sent %q", got)
	}
	if sessions.builds(next) != 1 {
		t.Fatalf("new session's backend built %d times, want 1", sessions.builds(next))
	}
	info, err := r.Info(ctx, "conv_conformance")
	if err != nil || info.RunnerRef[:len(next)] != next {
		t.Fatalf("Info after the move = %+v, %v", info, err)
	}
	if sessions.builds(next) != 1 {
		t.Fatal("Info rebuilt a backend that was already bound")
	}
}
